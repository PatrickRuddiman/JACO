package stagefirst_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/ingress/stagefirst"
)

func TestIsProdCA(t *testing.T) {
	cases := map[string]bool{
		"":                                 true, // default → prod
		stagefirst.LEProdCA:                true,
		stagefirst.LEStagingCA:             false,
		"https://acme.zerossl.com/v2/DV90": false,
		"https://pebble:14000/dir":         false,
	}
	for ca, want := range cases {
		if got := stagefirst.IsProdCA(ca); got != want {
			t.Errorf("IsProdCA(%q) = %v, want %v", ca, got, want)
		}
	}
}

func TestShouldStage(t *testing.T) {
	cases := []struct {
		name string
		p    stagefirst.Params
		want bool
	}{
		{"new domain on prod stages", stagefirst.Params{Domain: "a.com"}, true},
		{"skip-staging opts out", stagefirst.Params{Domain: "a.com", SkipStaging: true}, false},
		{"non-prod CA skips", stagefirst.Params{Domain: "a.com", ConfiguredCA: stagefirst.LEStagingCA}, false},
		{"already issued skips", stagefirst.Params{Domain: "a.com", AlreadyIssued: true}, false},
		{"explicit prod CA stages", stagefirst.Params{Domain: "a.com", ConfiguredCA: stagefirst.LEProdCA}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stagefirst.ShouldStage(c.p).Stage; got != c.want {
				t.Errorf("ShouldStage(%+v).Stage = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

// makeLeaf builds a self-signed leaf with the given SAN for self-check tests.
func makeLeaf(t *testing.T, san string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: san},
		DNSNames:     []string{san},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestSelfCheck(t *testing.T) {
	good := makeLeaf(t, "example.com")
	if err := stagefirst.SelfCheck("example.com", good); err != nil {
		t.Errorf("SelfCheck good chain: %v", err)
	}
	if err := stagefirst.SelfCheck("other.com", good); err == nil {
		t.Errorf("SelfCheck should reject SAN mismatch")
	}
	if err := stagefirst.SelfCheck("example.com", nil); err == nil {
		t.Errorf("SelfCheck should reject empty chain")
	}
	if err := stagefirst.SelfCheck("example.com", []byte("not pem")); err == nil {
		t.Errorf("SelfCheck should reject non-PEM input")
	}
}

func TestController_StagesNewDomainThenPromotes(t *testing.T) {
	var stagingChain []byte
	var promoted []string
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(domain string) ([]byte, bool) {
			if stagingChain == nil {
				return nil, false
			}
			return stagingChain, true
		},
		IssuedProd:  func(string) bool { return false },
		OnPromote:   func(d string) { promoted = append(promoted, d) },
		OnStageFail: func(string, error) { t.Errorf("unexpected stage fail") },
		Now:         time.Now,
	}
	ctx := context.Background()

	// First reconcile: new domain → staging.
	if !ctrl.Reconcile(ctx, []string{"new.example.com"}) {
		t.Fatalf("first reconcile should mark domain staging (changed=true)")
	}
	if !ctrl.StagingDomains()["new.example.com"] {
		t.Fatalf("domain not in staging set")
	}

	// Staging cert hasn't landed yet → no change, still staging.
	if ctrl.Reconcile(ctx, []string{"new.example.com"}) {
		t.Errorf("reconcile changed before staging chain landed")
	}

	// Staging cert lands + passes self-check → promote.
	stagingChain = makeLeaf(t, "new.example.com")
	if !ctrl.Reconcile(ctx, []string{"new.example.com"}) {
		t.Fatalf("reconcile should promote (changed=true)")
	}
	if ctrl.StagingDomains()["new.example.com"] {
		t.Errorf("domain still staging after promote")
	}
	if len(promoted) != 1 || promoted[0] != "new.example.com" {
		t.Errorf("promoted = %v, want [new.example.com]", promoted)
	}
}

func TestController_StageFailBacksOff(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	var fails int
	ctrl := &stagefirst.Controller{
		// A SAN-mismatched chain fails the self-check.
		LoadStagingChain: func(string) ([]byte, bool) { return makeLeaf(t, "wrong.com"), true },
		IssuedProd:       func(string) bool { return false },
		OnPromote:        func(string) { t.Errorf("should not promote on self-check fail") },
		OnStageFail:      func(string, error) { fails++ },
		Now:              clock,
	}
	ctx := context.Background()

	// Stage, then the bad chain fails the self-check → backoff.
	ctrl.Reconcile(ctx, []string{"bad.example.com"})
	ctrl.Reconcile(ctx, []string{"bad.example.com"})
	if fails != 1 {
		t.Fatalf("OnStageFail called %d times, want 1", fails)
	}
	if ctrl.StagingDomains()["bad.example.com"] {
		t.Errorf("domain should be dropped from staging after self-check fail")
	}
	// Within the backoff window, the domain is NOT re-staged.
	if ctrl.Reconcile(ctx, []string{"bad.example.com"}) {
		t.Errorf("domain re-staged inside backoff window")
	}
	// After the window elapses, it re-stages.
	now = now.Add(stagefirst.BackoffWindow + time.Minute)
	if !ctrl.Reconcile(ctx, []string{"bad.example.com"}) {
		t.Errorf("domain not re-staged after backoff window")
	}
}

func TestController_SkipStagingNeverStages(t *testing.T) {
	ctrl := &stagefirst.Controller{SkipStaging: true, IssuedProd: func(string) bool { return false }}
	if ctrl.Reconcile(context.Background(), []string{"a.com"}) {
		t.Errorf("skip-staging controller marked a domain staging")
	}
	if len(ctrl.StagingDomains()) != 0 {
		t.Errorf("staging set non-empty with skip-staging")
	}
}

// TestController_PromotedDomainNotReStagedDuringPendingWindow pins the fix
// for issue #154. After a successful staging→prod promotion the controller
// MUST refuse to re-stage the domain until either the prod cert lands
// (IssuedProd returns true) or PendingProdWindow elapses. Pre-fix the next
// Reconcile tick would see prodCertIssued=false (Caddy is still working
// the ACME order) and ShouldStage=true, re-adding the domain to staging
// and forcing Caddy to abandon the in-flight prod order — a flip-flop
// that never let any prod cert land. The operator-visible signature was
// "promoting to prod" logged every 10s tick forever per staging cert.
func TestController_PromotedDomainNotReStagedDuringPendingWindow(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	// Caddy hasn't completed prod issuance yet — IssuedProd stays false
	// across every Reconcile in this test, just like the operator's live
	// repro where the flip-flop loop never gave Caddy enough time.
	prodIssued := false
	var promoted []string
	stagingChain := makeLeaf(t, "new.example.com")

	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return prodIssued },
		OnPromote:        func(d string) { promoted = append(promoted, d) },
		OnStageFail:      func(string, error) { t.Errorf("unexpected stage fail") },
		Now:              clock,
	}
	ctx := context.Background()
	domains := []string{"new.example.com"}

	// Tick 1: ShouldStage=true (new domain) → added to staging set.
	ctrl.Reconcile(ctx, domains)
	if !ctrl.StagingDomains()["new.example.com"] {
		t.Fatalf("tick 1: domain not in staging set")
	}

	// Tick 2: staging chain ready, self-check passes → promote. OnPromote
	// fires once; domain dropped from staging set.
	ctrl.Reconcile(ctx, domains)
	if got := len(promoted); got != 1 {
		t.Fatalf("tick 2: promoted=%d, want 1", got)
	}
	if ctrl.StagingDomains()["new.example.com"] {
		t.Fatalf("tick 2: domain still in staging after promote")
	}

	// Ticks 3..N within the pending window: the flip-flop replays.
	// Pre-fix every one of these would re-add the domain to staging and
	// fire OnPromote on the *following* tick — so OnPromote count would
	// grow without bound. Post-fix the pending-prod guard short-circuits
	// at the top of the "not staging yet" branch and the domain stays
	// out of staging.
	for i := 0; i < 10; i++ {
		now = now.Add(10 * time.Second) // simulate the 10s reconcile tick
		ctrl.Reconcile(ctx, domains)
		if ctrl.StagingDomains()["new.example.com"] {
			t.Errorf("tick %d: domain re-added to staging during pending window (#154 regression)", 3+i)
		}
	}
	if got := len(promoted); got != 1 {
		t.Errorf("OnPromote fired %d times across pending window; want exactly 1 (#154 regression — would have been %d+ pre-fix)",
			got, 1+10)
	}

	// Prod cert lands → next Reconcile clears the pendingProd marker.
	// From here on ShouldStage's "already issued" rule (#3) keeps the
	// domain out of staging permanently.
	prodIssued = true
	now = now.Add(10 * time.Second)
	ctrl.Reconcile(ctx, domains)
	if ctrl.StagingDomains()["new.example.com"] {
		t.Errorf("domain in staging after prod cert landed")
	}
	if got := len(promoted); got != 1 {
		t.Errorf("OnPromote fired %d times after prod cert landed; want 1 (no new promote)", got)
	}
}

// TestController_PendingWindowExpiresWithoutProdCertReStages pins the
// other half of the #154 contract: if Caddy genuinely fails to complete
// the prod ACME order within PendingProdWindow, the controller MUST let
// ShouldStage retry the dry-run from scratch — otherwise a real prod
// failure would never get a fresh attempt and the domain would silently
// stay on staging-only forever.
func TestController_PendingWindowExpiresWithoutProdCertReStages(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	var promoted []string
	stagingChain := makeLeaf(t, "expired.example.com")
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return false }, // never lands
		OnPromote:        func(d string) { promoted = append(promoted, d) },
		OnStageFail:      func(string, error) { t.Errorf("unexpected stage fail") },
		Now:              clock,
	}
	ctx := context.Background()
	domains := []string{"expired.example.com"}

	// Stage + promote, putting the domain in the pending window.
	ctrl.Reconcile(ctx, domains) // tick 1: stage
	ctrl.Reconcile(ctx, domains) // tick 2: promote
	if len(promoted) != 1 {
		t.Fatalf("setup: expected one promote, got %d", len(promoted))
	}

	// Jump past the pending window. Next Reconcile MUST clear the marker
	// and let ShouldStage re-decide. Since IssuedProd is still false and
	// the domain has no backoff, ShouldStage returns Stage=true and the
	// dry-run retries — which is the right behavior on a real prod
	// failure (DNS regressed, CA outage, etc.).
	now = now.Add(stagefirst.PendingProdWindow + time.Minute)
	ctrl.Reconcile(ctx, domains)
	if !ctrl.StagingDomains()["expired.example.com"] {
		t.Errorf("after expired window: expected re-stage, but domain not in staging set")
	}
}
