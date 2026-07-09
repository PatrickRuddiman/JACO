package challenge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/mholt/acmez/v3/acme"

	"github.com/PatrickRuddiman/jaco/internal/ingress/challenge"
)

// TestPublishToken_StoresWithoutAudit pins issue #189: the storage tap
// republishes every CertMagic challenge-token write through PublishToken,
// which must land the token in state.ChallengeTokens (so any node can serve
// the HTTP-01 validation) but must NOT emit an audit event — CertMagic writes
// one blob per order attempt, and a CERTIFICATE_RENEWED per write would spam
// the audit log (that single intentional pair is Issue's job).
func TestPublishToken_StoresWithoutAudit(t *testing.T) {
	apply, st := newHarness(t)
	iss := challenge.NewIssuer(apply)

	if err := iss.PublishToken(context.Background(), "web.example.com", "tok123", "tok123.keyauth"); err != nil {
		t.Fatalf("PublishToken: %v", err)
	}

	ct, ok := st.ChallengeTokens.Get("tok123")
	if !ok {
		t.Fatalf("token not stored in state.ChallengeTokens")
	}
	if ct.GetKeyAuth() != "tok123.keyauth" {
		t.Errorf("key_auth = %q, want tok123.keyauth", ct.GetKeyAuth())
	}
	if ct.GetDomain() != "web.example.com" {
		t.Errorf("domain = %q, want web.example.com", ct.GetDomain())
	}
	if n := len(st.AuditEvents.List()); n != 0 {
		t.Errorf("PublishToken emitted %d audit events; want 0 (would spam the log)", n)
	}
}

func TestPublishToken_RejectsEmptyFields(t *testing.T) {
	apply, _ := newHarness(t)
	iss := challenge.NewIssuer(apply)
	if err := iss.PublishToken(context.Background(), "", "tok", "auth"); err == nil {
		t.Errorf("PublishToken with empty domain should error")
	}
	if err := iss.PublishToken(context.Background(), "d", "", "auth"); err == nil {
		t.Errorf("PublishToken with empty token should error")
	}
	if err := iss.PublishToken(context.Background(), "d", "tok", ""); err == nil {
		t.Errorf("PublishToken with empty keyAuth should error")
	}
}

// TestParseHTTP01Blob covers the JSON CertMagic stores under its
// `.../challenge_tokens/<domain>.json` key (a marshaled acme.Challenge). The
// primary case marshals a real acmez acme.Challenge so the parser stays
// coupled to the exact wire format certmagic produces; the rest pin the
// reject paths so the storage tap ignores tls-alpn-01 / dns-01 and malformed
// blobs.
func TestParseHTTP01Blob(t *testing.T) {
	realHTTP01, err := json.Marshal(acme.Challenge{
		Type:             acme.ChallengeTypeHTTP01,
		Token:            "tokA",
		KeyAuthorization: "tokA.thumbprint",
		Identifier:       acme.Identifier{Type: "dns", Value: "web.example.com"},
	})
	if err != nil {
		t.Fatalf("marshal acme.Challenge: %v", err)
	}

	t.Run("valid_http01_from_acmez", func(t *testing.T) {
		domain, token, keyAuth, ok := challenge.ParseHTTP01Blob(realHTTP01)
		if !ok {
			t.Fatalf("ParseHTTP01Blob(real http-01) ok=false; want true")
		}
		if domain != "web.example.com" || token != "tokA" || keyAuth != "tokA.thumbprint" {
			t.Errorf("got (%q,%q,%q); want (web.example.com, tokA, tokA.thumbprint)", domain, token, keyAuth)
		}
	})

	rejects := map[string]string{
		"tls-alpn-01 ignored": `{"type":"tls-alpn-01","token":"t","keyAuthorization":"t.k","identifier":{"type":"dns","value":"web.example.com"}}`,
		"dns-01 ignored":      `{"type":"dns-01","token":"t","keyAuthorization":"t.k","identifier":{"type":"dns","value":"web.example.com"}}`,
		"missing token":       `{"type":"http-01","keyAuthorization":"t.k","identifier":{"type":"dns","value":"web.example.com"}}`,
		"missing keyAuth":     `{"type":"http-01","token":"t","identifier":{"type":"dns","value":"web.example.com"}}`,
		"missing identifier":  `{"type":"http-01","token":"t","keyAuthorization":"t.k"}`,
		"garbage":             `not json at all`,
		"empty":               ``,
	}
	for name, blob := range rejects {
		t.Run(name, func(t *testing.T) {
			if _, _, _, ok := challenge.ParseHTTP01Blob([]byte(blob)); ok {
				t.Errorf("ParseHTTP01Blob(%q) ok=true; want false", blob)
			}
		})
	}
}

// TestCaddyHandler_ServesTokenByValue is the end-to-end of the serve side:
// the jaco_acme_challenge Caddy module, reading the shared state singleton,
// returns 200 + key_auth for a replicated token and 404 for an unknown one —
// keyed purely by token, so it answers regardless of which CA policy the node
// renders (the #189 fix). It is terminal: next must never be called.
func TestCaddyHandler_ServesTokenByValue(t *testing.T) {
	apply, st := newHarness(t)
	challenge.SetDefaultChallengeState(st)
	defer challenge.SetDefaultChallengeState(nil)

	if err := challenge.NewIssuer(apply).PublishToken(context.Background(), "web.example.com", "tok123", "tok123.keyauth"); err != nil {
		t.Fatalf("PublishToken: %v", err)
	}

	var mod challenge.CaddyHandler
	terminalNext := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatalf("challenge handler must be terminal; next was called")
		return nil
	})

	t.Run("known token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, challenge.ChallengePath+"tok123", nil)
		if err := mod.ServeHTTP(rec, req, terminalNext); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d; want 200", rec.Code)
		}
		if rec.Body.String() != "tok123.keyauth" {
			t.Errorf("body = %q; want tok123.keyauth", rec.Body.String())
		}
	})

	t.Run("unknown token 404s", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, challenge.ChallengePath+"nope", nil)
		if err := mod.ServeHTTP(rec, req, terminalNext); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rec.Code)
		}
	})
}

// TestCaddyHandler_NilStateIs404 guards the misconfigured path: if the daemon
// never called SetDefaultChallengeState the module must 404 rather than panic
// or proxy the challenge path onward.
func TestCaddyHandler_NilStateIs404(t *testing.T) {
	challenge.SetDefaultChallengeState(nil)
	var mod challenge.CaddyHandler
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, challenge.ChallengePath+"tok", nil)
	if err := mod.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatalf("must not call next when state is nil")
		return nil
	})); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}
