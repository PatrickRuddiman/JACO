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
// the prod ACME order within PendingProdWindow, the controller MUST enter
// a prodBackoff window (issue #189) and re-stage after that window expires.
// Without the backoff the pendingProd-expire→re-stage→re-promote loop would
// fire a prod ACME order every ~5 min, extending LE's failed-auth window.
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

	// Jump past the pending window. The controller enters prodBackoff (#189)
	// and must NOT immediately re-stage.
	now = now.Add(stagefirst.PendingProdWindow + time.Minute)
	ctrl.Reconcile(ctx, domains) // window expires → prodBackoff starts
	if ctrl.StagingDomains()["expired.example.com"] {
		t.Errorf("domain re-staged immediately after prod failure; want prodBackoff active (#189)")
	}

	// While prodBackoff is active, the domain stays out of staging.
	now = now.Add(stagefirst.ProdBackoffBase - 2*time.Minute)
	ctrl.Reconcile(ctx, domains)
	if ctrl.StagingDomains()["expired.example.com"] {
		t.Errorf("domain re-staged while prodBackoff active (#189)")
	}

	// After prodBackoff expires, ShouldStage retries the dry-run from scratch.
	now = now.Add(stagefirst.ProdBackoffBase + time.Minute)
	ctrl.Reconcile(ctx, domains)
	if !ctrl.StagingDomains()["expired.example.com"] {
		t.Errorf("after prodBackoff expired: expected re-stage, but domain not in staging set (#189)")
	}
}

// TestController_PromoteFiresClearStagingCert pins issue #158: when a
// domain is promoted from staging→prod, the controller MUST invoke the
// ClearStagingCert callback exactly once for that domain BEFORE OnPromote
// fires (so the storage wipe happens before the rebuild that would
// otherwise let certmagic keep serving the cached staging leaf). Pre-fix
// no such callback existed and the staging cert blob sat in raft for its
// full 90-day validity, blocking prod issuance and forcing the operator
// to `rm -rf /var/lib/jaco/ingress/cache` by hand.
func TestController_PromoteFiresClearStagingCert(t *testing.T) {
	stagingChain := makeLeaf(t, "new.example.com")
	var clearedOrder []string
	var promotedOrder []string
	var seq []string
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return false },
		ClearStagingCert: func(d string) {
			clearedOrder = append(clearedOrder, d)
			seq = append(seq, "clear:"+d)
		},
		OnPromote: func(d string) {
			promotedOrder = append(promotedOrder, d)
			seq = append(seq, "promote:"+d)
		},
		OnStageFail: func(string, error) { t.Errorf("unexpected stage fail") },
		Now:         time.Now,
	}
	ctx := context.Background()
	domains := []string{"new.example.com"}

	// Tick 1: stage.
	ctrl.Reconcile(ctx, domains)
	// Tick 2: promote → ClearStagingCert + OnPromote in that order.
	ctrl.Reconcile(ctx, domains)

	if got, want := clearedOrder, []string{"new.example.com"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("ClearStagingCert calls = %v, want %v (#158 regression)", got, want)
	}
	if got, want := promotedOrder, []string{"new.example.com"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("OnPromote calls = %v, want %v", got, want)
	}
	if len(seq) != 2 || seq[0] != "clear:new.example.com" || seq[1] != "promote:new.example.com" {
		t.Errorf("ClearStagingCert must fire BEFORE OnPromote; got %v (#158)", seq)
	}

	// Re-issue is a one-shot per promotion: extra reconcile ticks during the
	// pending-prod window MUST NOT re-fire ClearStagingCert (otherwise we'd
	// thrash storage every tick).
	for i := 0; i < 5; i++ {
		ctrl.Reconcile(ctx, domains)
	}
	if len(clearedOrder) != 1 {
		t.Errorf("ClearStagingCert fired %d times across pending window; want exactly 1 (#158)", len(clearedOrder))
	}
}

// TestController_PromoteWithoutClearStagingCertSafe pins back-compat for
// callers that don't wire ClearStagingCert: OnPromote still fires and the
// promote path doesn't panic when the callback is nil. Without this nil
// guard the daemon would crash the first time any tls:auto domain promoted.
func TestController_PromoteWithoutClearStagingCertSafe(t *testing.T) {
	stagingChain := makeLeaf(t, "new.example.com")
	var promoted int
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return false },
		OnPromote:        func(string) { promoted++ },
		Now:              time.Now,
		// ClearStagingCert deliberately left nil.
	}
	ctx := context.Background()
	ctrl.Reconcile(ctx, []string{"new.example.com"}) // stage
	ctrl.Reconcile(ctx, []string{"new.example.com"}) // promote
	if promoted != 1 {
		t.Errorf("OnPromote fired %d times; want 1", promoted)
	}
}

// TestController_OnProdIssuedFiresOnceWhenProdLands pins issue #147: when
// the controller observes a prod cert landing in raft for a previously
// promoted (pending-prod) domain, it MUST fire OnProdIssued exactly once.
// The daemon hooks this to emit CERTIFICATE_ISSUED(prod), without which
// `jaco status` reports `ENVIRONMENT staging` forever because the only
// CERTIFICATE_ISSUED audit event ever emitted is the staging one fired
// from OnPromote.
func TestController_OnProdIssuedFiresOnceWhenProdLands(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	prodIssued := false
	stagingChain := makeLeaf(t, "new.example.com")
	var prodHookCalls []string
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return prodIssued },
		OnPromote:        func(string) {},
		OnProdIssued:     func(d string) { prodHookCalls = append(prodHookCalls, d) },
		Now:              clock,
	}
	ctx := context.Background()
	domains := []string{"new.example.com"}

	ctrl.Reconcile(ctx, domains) // stage
	ctrl.Reconcile(ctx, domains) // promote (pending)

	// Inside the pending window, prod hasn't landed yet — OnProdIssued
	// MUST stay silent.
	for i := 0; i < 3; i++ {
		now = now.Add(10 * time.Second)
		ctrl.Reconcile(ctx, domains)
	}
	if len(prodHookCalls) != 0 {
		t.Fatalf("OnProdIssued fired %d times before prod cert landed; want 0", len(prodHookCalls))
	}

	// Prod cert lands → next Reconcile flips pendingProd → fires OnProdIssued
	// exactly once with the right domain.
	prodIssued = true
	now = now.Add(10 * time.Second)
	ctrl.Reconcile(ctx, domains)
	if got, want := prodHookCalls, []string{"new.example.com"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("OnProdIssued calls = %v, want %v (#147)", got, want)
	}

	// And NEVER fires again — subsequent ticks see IssuedProd=true but the
	// pendingProd marker was already cleared, so the switch arm doesn't run.
	for i := 0; i < 5; i++ {
		now = now.Add(10 * time.Second)
		ctrl.Reconcile(ctx, domains)
	}
	if len(prodHookCalls) != 1 {
		t.Errorf("OnProdIssued re-fired across later ticks (%d total); want exactly 1 per promotion (#147)", len(prodHookCalls))
	}
}

// TestController_ProdRaceConvergesLeaderOffStaging pins the #189 follower
// race: a domain is still in this node's staging set when a PEER (rendering
// the prod CA behind an L4 load balancer) wins prod issuance. The next
// Reconcile MUST observe IssuedProd=true, drop the domain from staging,
// fire OnProdIssued exactly once, and never re-stage it — otherwise the
// leader stays pinned to the staging CA forever and never serves the prod
// leaf that already exists cluster-wide.
func TestController_ProdRaceConvergesLeaderOffStaging(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	prodIssued := false
	var prodHookCalls []string
	ctrl := &stagefirst.Controller{
		// Staging cert never lands — only the peer's prod cert does.
		LoadStagingChain: func(string) ([]byte, bool) { return nil, false },
		IssuedProd:       func(string) bool { return prodIssued },
		OnPromote:        func(string) { t.Errorf("unexpected OnPromote: no staging self-check happened") },
		OnProdIssued:     func(d string) { prodHookCalls = append(prodHookCalls, d) },
		Now:              clock,
	}
	ctx := context.Background()
	domains := []string{"new.example.com"}

	// First reconcile stages the new domain.
	if !ctrl.Reconcile(ctx, domains) {
		t.Fatalf("first reconcile should stage the domain")
	}
	if !ctrl.StagingDomains()["new.example.com"] {
		t.Fatalf("domain not in staging set after first reconcile")
	}

	// Staging cert never lands; ticks while staging produce no change.
	for i := 0; i < 3; i++ {
		now = now.Add(10 * time.Second)
		if ctrl.Reconcile(ctx, domains) {
			t.Fatalf("reconcile changed while staging cert absent and prod not yet landed")
		}
	}

	// Peer wins prod issuance behind the LB → next tick converges off staging.
	prodIssued = true
	now = now.Add(10 * time.Second)
	if !ctrl.Reconcile(ctx, domains) {
		t.Fatalf("reconcile should converge off staging when prod cert lands (changed=true)")
	}
	if ctrl.StagingDomains()["new.example.com"] {
		t.Errorf("domain still staging after prod cert observed")
	}
	if got, want := prodHookCalls, []string{"new.example.com"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("OnProdIssued calls = %v, want %v (#189)", got, want)
	}

	// And NEVER re-stages or re-fires: ShouldStage(AlreadyIssued=true) keeps
	// it out of staging permanently and OnProdIssued stays at one.
	for i := 0; i < 5; i++ {
		now = now.Add(10 * time.Second)
		if ctrl.Reconcile(ctx, domains) {
			t.Errorf("reconcile re-staged a domain that already holds a prod cert (#189)")
		}
	}
	if len(prodHookCalls) != 1 {
		t.Errorf("OnProdIssued re-fired (%d total); want exactly 1 (#189)", len(prodHookCalls))
	}
}

// TestController_PendingWindowExpiryDoesNotFireOnProdIssued pins the
// negative of #147: if the prod ACME order genuinely fails (window
// expires without prodCertIssued flipping true), OnProdIssued MUST NOT
// fire. Otherwise the audit trail would falsely claim a prod cert was
// issued.
func TestController_PendingWindowExpiryDoesNotFireOnProdIssued(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	stagingChain := makeLeaf(t, "expired.example.com")
	var prodHookCalls int
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return false }, // prod never lands
		OnPromote:        func(string) {},
		OnProdIssued:     func(string) { prodHookCalls++ },
		Now:              clock,
	}
	ctx := context.Background()
	domains := []string{"expired.example.com"}

	ctrl.Reconcile(ctx, domains) // stage
	ctrl.Reconcile(ctx, domains) // promote
	now = now.Add(stagefirst.PendingProdWindow + time.Minute)
	ctrl.Reconcile(ctx, domains) // window expires

	if prodHookCalls != 0 {
		t.Errorf("OnProdIssued fired %d times after window expiry; want 0 (#147)", prodHookCalls)
	}
}

// TestController_ProdFailureBacksOff verifies issue #189 fix B: after the
// PendingProdWindow expires without a prod cert, the controller backs off
// exponentially (15m base), suppresses re-staging during that window, then
// re-stages when the backoff expires.
func TestController_ProdFailureBacksOff(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	stagingChain := makeLeaf(t, "stuck.example.com")

	var prodFailDomains []string
	var prodFailDurations []time.Duration
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return false }, // prod never lands
		OnPromote:        func(string) {},
		OnStageFail:      func(string, error) { t.Errorf("unexpected stage fail") },
		OnProdFail: func(domain string, retryAfter time.Duration) {
			prodFailDomains = append(prodFailDomains, domain)
			prodFailDurations = append(prodFailDurations, retryAfter)
		},
		Now: clock,
	}
	ctx := context.Background()
	domains := []string{"stuck.example.com"}

	// Tick 1: stage; tick 2: promote → pendingProd.
	ctrl.Reconcile(ctx, domains)
	ctrl.Reconcile(ctx, domains)

	// Advance past PendingProdWindow — window expires this tick.
	now = now.Add(stagefirst.PendingProdWindow + time.Second)
	ctrl.Reconcile(ctx, domains)

	// OnProdFail must have fired exactly once with 15m (#189).
	if len(prodFailDomains) != 1 || prodFailDomains[0] != "stuck.example.com" {
		t.Fatalf("OnProdFail calls = %v, want [stuck.example.com]", prodFailDomains)
	}
	if prodFailDurations[0] != stagefirst.ProdBackoffBase {
		t.Errorf("OnProdFail duration = %v, want %v (#189)", prodFailDurations[0], stagefirst.ProdBackoffBase)
	}

	// Domain must NOT be re-staged within the prodBackoff window.
	if ctrl.StagingDomains()["stuck.example.com"] {
		t.Errorf("domain re-staged immediately after prod failure (#189 regression)")
	}
	now = now.Add(stagefirst.ProdBackoffBase - 2*time.Minute)
	ctrl.Reconcile(ctx, domains)
	if ctrl.StagingDomains()["stuck.example.com"] {
		t.Errorf("domain re-staged while prodBackoff still active (#189 regression)")
	}

	// After the prodBackoff window expires, the domain re-stages.
	now = now.Add(stagefirst.ProdBackoffBase + time.Minute)
	if !ctrl.Reconcile(ctx, domains) {
		t.Errorf("expected re-stage after prodBackoff expires (#189)")
	}
	if !ctrl.StagingDomains()["stuck.example.com"] {
		t.Errorf("domain not re-staged after prodBackoff expires (#189)")
	}
}

// TestController_ProdSuccessResetsProdBackoff verifies that a successful prod
// cert resets the failure counter — a subsequent failure restarts at 15m, not
// 30m. Issue #189.
func TestController_ProdSuccessResetsProdBackoff(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	stagingChain := makeLeaf(t, "eventual.example.com")
	prodIssued := false

	var prodFailDurations []time.Duration
	ctrl := &stagefirst.Controller{
		LoadStagingChain: func(string) ([]byte, bool) { return stagingChain, true },
		IssuedProd:       func(string) bool { return prodIssued },
		OnPromote:        func(string) {},
		OnStageFail:      func(string, error) { t.Errorf("unexpected stage fail") },
		OnProdFail: func(_ string, d time.Duration) {
			prodFailDurations = append(prodFailDurations, d)
		},
		Now: clock,
	}
	ctx := context.Background()
	domains := []string{"eventual.example.com"}

	// First attempt: stage → promote → window expires → OnProdFail(15m).
	ctrl.Reconcile(ctx, domains)
	ctrl.Reconcile(ctx, domains)
	now = now.Add(stagefirst.PendingProdWindow + time.Second)
	ctrl.Reconcile(ctx, domains)
	if len(prodFailDurations) != 1 || prodFailDurations[0] != stagefirst.ProdBackoffBase {
		t.Fatalf("setup: want first fail=15m, got %v", prodFailDurations)
	}

	// Let prodBackoff expire; re-stage + re-promote; this time prod cert lands.
	now = now.Add(stagefirst.ProdBackoffBase + time.Minute)
	ctrl.Reconcile(ctx, domains) // re-stage
	ctrl.Reconcile(ctx, domains) // re-promote → pending

	// Prod cert lands within the window.
	prodIssued = true
	now = now.Add(10 * time.Second)
	ctrl.Reconcile(ctx, domains) // prod issued → resets prodFails

	// Now simulate another failure cycle — prodFails must have been reset,
	// so the next failure should backoff for 15m again, not 30m.
	prodIssued = false
	// With prodIssued now false and no staging/pending/backoff entry,
	// ShouldStage returns true → domain re-stages.
	now = now.Add(time.Second)
	ctrl.Reconcile(ctx, domains) // stage
	ctrl.Reconcile(ctx, domains) // promote
	now = now.Add(stagefirst.PendingProdWindow + time.Second)
	ctrl.Reconcile(ctx, domains) // window expires → OnProdFail

	if len(prodFailDurations) != 2 {
		t.Fatalf("expected 2 OnProdFail calls, got %d", len(prodFailDurations))
	}
	// After the reset, the second failure must be 15m (base), not 30m.
	if prodFailDurations[1] != stagefirst.ProdBackoffBase {
		t.Errorf("second failure = %v, want %v (counter not reset on prod success, #189)", prodFailDurations[1], stagefirst.ProdBackoffBase)
	}
}
