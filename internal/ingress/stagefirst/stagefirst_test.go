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
