package config_test

import (
	"encoding/json"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/ingress/config"
)

// Per-stack acme_email (#102): when routes carry distinct ACMEEmail
// values, buildTLSPolicies emits a separate automation policy / issuer per
// (staging, effective-email) tuple. A route with empty ACMEEmail falls
// back to opts.ACMEEmail (cluster default), so legacy clusters that don't
// set per-stack emails keep their historical single-policy shape.

func policiesFromConfig(t *testing.T, cfg []byte) []any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatalf("parse caddy json: %v", err)
	}
	tls, ok := parsed["apps"].(map[string]any)["tls"].(map[string]any)
	if !ok {
		t.Fatal("no apps.tls section")
	}
	return tls["automation"].(map[string]any)["policies"].([]any)
}

// emailFor builds a (domain → email) map across every policy in the rendered
// config so a test can assert "domain X registers under email Y".
func emailFor(t *testing.T, policies []any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pAny := range policies {
		p := pAny.(map[string]any)
		issuer := p["issuers"].([]any)[0].(map[string]any)
		email, _ := issuer["email"].(string)
		for _, s := range p["subjects"].([]any) {
			out[s.(string)] = email
		}
	}
	return out
}

func TestBuildCaddyConfig_AcmeEmail_PerStackDistinctPolicies(t *testing.T) {
	routes := []config.Route{
		{Domain: "app.team-a.example.com", Deployment: "team-a", Service: "web", Port: 80, TLSAuto: true, ACMEEmail: "a@example.com"},
		{Domain: "app.team-b.example.com", Deployment: "team-b", Service: "web", Port: 80, TLSAuto: true, ACMEEmail: "b@example.com"},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	policies := policiesFromConfig(t, got)
	if len(policies) != 2 {
		t.Fatalf("policies = %d, want 2 (one per distinct email)", len(policies))
	}
	got2 := emailFor(t, policies)
	if got2["app.team-a.example.com"] != "a@example.com" {
		t.Errorf("team-a registered under %q, want a@example.com", got2["app.team-a.example.com"])
	}
	if got2["app.team-b.example.com"] != "b@example.com" {
		t.Errorf("team-b registered under %q, want b@example.com", got2["app.team-b.example.com"])
	}
}

func TestBuildCaddyConfig_AcmeEmail_EmptyFallsBackToClusterDefault(t *testing.T) {
	routes := []config.Route{
		{Domain: "fallback.example.com", Deployment: "noemail", Service: "w", Port: 80, TLSAuto: true /* ACMEEmail unset */},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	policies := policiesFromConfig(t, got)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1", len(policies))
	}
	got2 := emailFor(t, policies)
	// opts().ACMEEmail = "ops@example.com" (cluster default).
	if got2["fallback.example.com"] != "ops@example.com" {
		t.Errorf("fallback registered under %q, want ops@example.com (cluster default)", got2["fallback.example.com"])
	}
}

func TestBuildCaddyConfig_AcmeEmail_AllEmptyKeepsSinglePolicy(t *testing.T) {
	// Back-compat: a cluster with no per-stack emails set must emit exactly
	// one automation policy (the historical shape), with all domains under
	// the cluster default email.
	routes := []config.Route{
		{Domain: "one.example.com", Deployment: "x", Service: "w", Port: 80, TLSAuto: true},
		{Domain: "two.example.com", Deployment: "y", Service: "w", Port: 80, TLSAuto: true},
		{Domain: "three.example.com", Deployment: "z", Service: "w", Port: 80, TLSAuto: true},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	policies := policiesFromConfig(t, got)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1 (no per-stack emails ⇒ single policy)", len(policies))
	}
	subjects := policies[0].(map[string]any)["subjects"].([]any)
	if len(subjects) != 3 {
		t.Errorf("subjects = %d, want 3", len(subjects))
	}
	issuer := policies[0].(map[string]any)["issuers"].([]any)[0].(map[string]any)
	if issuer["email"] != "ops@example.com" {
		t.Errorf("issuer email = %v, want ops@example.com", issuer["email"])
	}
}

func TestBuildCaddyConfig_AcmeEmail_SharedEmailCoalesces(t *testing.T) {
	// Two stacks with the SAME per-stack email collapse into one policy
	// (the grouping key is (staging, email) — same key, same policy).
	routes := []config.Route{
		{Domain: "a.shared.example.com", Deployment: "p", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "shared@example.com"},
		{Domain: "b.shared.example.com", Deployment: "q", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "shared@example.com"},
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, opts())
	if err != nil {
		t.Fatal(err)
	}
	policies := policiesFromConfig(t, got)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1 (shared email coalesces)", len(policies))
	}
	subjects := policies[0].(map[string]any)["subjects"].([]any)
	if len(subjects) != 2 {
		t.Errorf("subjects = %d, want 2", len(subjects))
	}
	issuer := policies[0].(map[string]any)["issuers"].([]any)[0].(map[string]any)
	if issuer["email"] != "shared@example.com" {
		t.Errorf("issuer email = %v, want shared@example.com", issuer["email"])
	}
}

func TestBuildCaddyConfig_AcmeEmail_StagingAndProdSplitByEmail(t *testing.T) {
	// Staging-first interacts with per-stack email: each (staging, email)
	// tuple gets its own policy. Two stacks in staging with different
	// emails → two staging policies; another stack on prod → its own
	// prod policy. Sort order: staging first, then prod, each by email.
	routes := []config.Route{
		{Domain: "staging-a.example.com", Deployment: "a", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "a@example.com"},
		{Domain: "staging-b.example.com", Deployment: "b", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "b@example.com"},
		{Domain: "prod-c.example.com", Deployment: "c", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "c@example.com"},
	}
	o := opts()
	o.ACMEStagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"
	o.StagingDomains = map[string]bool{
		"staging-a.example.com": true,
		"staging-b.example.com": true,
	}
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	policies := policiesFromConfig(t, got)
	if len(policies) != 3 {
		t.Fatalf("policies = %d, want 3 (two staging + one prod)", len(policies))
	}
	// Ordering invariant: every staging policy appears before any prod policy.
	for i, pAny := range policies {
		p := pAny.(map[string]any)
		ca := p["issuers"].([]any)[0].(map[string]any)["ca"].(string)
		isStaging := ca == o.ACMEStagingCA
		if !isStaging && i < 2 {
			t.Errorf("policy %d is prod CA but appears before all staging policies are emitted", i)
		}
	}
}

func TestBuildCaddyConfig_AcmeEmail_OmittedWhenACMEDisabled(t *testing.T) {
	// Per-stack acme_email must not resurrect the automation block when
	// cluster-wide acme_enabled is false.
	routes := []config.Route{
		{Domain: "x.example.com", Deployment: "x", Service: "w", Port: 80, TLSAuto: true, ACMEEmail: "x@example.com"},
	}
	o := opts()
	o.ACMEEnabled = false
	got, err := config.BuildCaddyConfig(routes, nil, nil, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	apps := parsed["apps"].(map[string]any)
	if _, hasTLS := apps["tls"]; hasTLS {
		t.Errorf("apps.tls present despite ACMEEnabled=false: %v", apps["tls"])
	}
}
