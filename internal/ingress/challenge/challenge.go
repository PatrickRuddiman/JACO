// Package challenge ships the two halves of HTTP-01 ACME challenge support:
//
//   - Issuer raft-Applies `Command{ChallengeTokenStore}` so every node sees
//     the token via state.ChallengeTokens. CertMagic on the issuing node
//     calls Issue() right before submitting the order; once the token lands
//     in raft, any node fielding the upstream's ACME validation request can
//     serve it.
//
//   - Handler is a stateless HTTP handler that matches
//     `/.well-known/acme-challenge/<token>` and returns the `key_auth` for
//     the matching ChallengeToken, or 404 when absent / expired. Plugs into
//     the embedded Caddy server's HTTP listener on :80.
//
// Both halves consult state directly so a node that just woke up via watch
// catch-up can serve a token without coordinating with the issuing node.
package challenge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// TokenTTL is how long a ChallengeToken stays valid post-issue. ACME
// validation typically completes within seconds; a 10-minute TTL covers
// retries + slow CAs.
const TokenTTL = 10 * time.Minute

// ChallengePath is the URL prefix ACME HTTP-01 validators GET against.
const ChallengePath = "/.well-known/acme-challenge/"

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// ACME environment labels carried on cert audit events so an operator can
// tell a cheap staging dry-run failure apart from a prod rate-limit hit
// (issue #41).
const (
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// Issuer publishes ChallengeTokens to raft and emits cert lifecycle audit
// events. The environment label (staging|prod) is stamped onto every audit
// payload so the stage-first flow is observable.
type Issuer struct {
	apply Applier
	env   string
}

// NewIssuer constructs an Issuer with the environment unspecified (prod-
// shaped events carry no acme_environment label). Prefer NewIssuerForEnv.
func NewIssuer(apply Applier) *Issuer { return &Issuer{apply: apply} }

// NewIssuerForEnv constructs an Issuer tagged with the ACME environment
// (EnvStaging | EnvProd) so emitted audit events carry acme_environment.
func NewIssuerForEnv(apply Applier, env string) *Issuer {
	return &Issuer{apply: apply, env: env}
}

// Issue raft-Applies a ChallengeTokenStore. domain + token + keyAuth come
// from certmagic's challenge presentation. Audit events for the CertMagic
// OnEvent pair land here too — CERTIFICATE_RENEWED on apply success,
// CERTIFICATE_FAILED on apply error. This is the closest signal we have
// without an embedded certmagic + OnEvent hook (the daemon execs an
// external caddy in v0).
func (i *Issuer) Issue(_ context.Context, domain, token, keyAuth string) error {
	if domain == "" || token == "" || keyAuth == "" {
		return fmt.Errorf("Issue: domain + token + keyAuth required")
	}
	now := time.Now()
	expiresAt := now.Add(TokenTTL)
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(now),
		Payload: &pb.Command_ChallengeTokenStore{ChallengeTokenStore: &pb.ChallengeTokenStore{
			Token: &pb.ChallengeToken{
				Token:     token,
				Domain:    domain,
				KeyAuth:   keyAuth,
				ExpiresAt: timestamppb.New(expiresAt),
			},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	if applyErr := i.apply(data); applyErr != nil {
		_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, i.withEnv(map[string]string{
			"domain": domain,
			"reason": applyErr.Error(),
		}))
		return applyErr
	}
	_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED, i.withEnv(map[string]string{
		"domain": domain,
	}))
	return nil
}

// FailureClass buckets an ACME issuance error so audit consumers and the
// stage-first decision logic can tell a transient problem (retry later)
// apart from a rate-limit (back off) apart from a config/DNS misconfig
// (don't bother escalating to prod).
type FailureClass string

const (
	// FailureRateLimit is the CA's rate-limit response — escalating to prod
	// would only burn the prod limit too, so the stage-first flow stops here.
	FailureRateLimit FailureClass = "rate_limit"
	// FailureValidation covers DNS / HTTP-01 reachability failures — the
	// cluster's routing or the operator's DNS is wrong; staging caught it
	// cheaply.
	FailureValidation FailureClass = "validation"
	// FailureTransient is a network blip / CA 5xx — safe to retry.
	FailureTransient FailureClass = "transient"
	// FailureUnknown is everything else.
	FailureUnknown FailureClass = "unknown"
)

// ClassifyFailure buckets an ACME error string. certmagic/acmez surface CA
// problem-detail types in the error text; we match on the stable substrings
// (LE's urn:ietf:params:acme:error:rateLimited etc.) rather than the wire
// type since the error is already flattened to a string by the time it
// reaches us.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailureUnknown
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "ratelimited") || strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many") || strings.Contains(s, "rate_limit"):
		return FailureRateLimit
	case strings.Contains(s, "dns") || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") ||
		strings.Contains(s, "timeout") && strings.Contains(s, "challenge") ||
		strings.Contains(s, "incorrect validation") || strings.Contains(s, "challenge failed"):
		return FailureValidation
	case strings.Contains(s, "503") || strings.Contains(s, "502") ||
		strings.Contains(s, "serverinternal") || strings.Contains(s, "temporarily"):
		return FailureTransient
	default:
		return FailureUnknown
	}
}

// EmitStageFailure records a CERTIFICATE_FAILED audit event for a staging-
// stage failure, stamping stage_failed_at=staging + the failure class so an
// operator sees that prod was deliberately NOT attempted (issue #41).
func (i *Issuer) EmitStageFailure(domain string, err error) {
	_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, map[string]string{
		"domain":           domain,
		"acme_environment": EnvStaging,
		"stage_failed_at":  "staging",
		"failure_class":    string(ClassifyFailure(err)),
		"reason":           err.Error(),
	})
}

// EmitIssued records a CERTIFICATE_ISSUED audit event tagged with the
// environment the cert was issued against.
func (i *Issuer) EmitIssued(domain, env string) {
	_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED, map[string]string{
		"domain":           domain,
		"acme_environment": env,
	})
}

// withEnv stamps acme_environment onto an audit payload when the issuer was
// constructed with a known environment.
func (i *Issuer) withEnv(payload map[string]string) map[string]string {
	if i.env != "" {
		payload["acme_environment"] = i.env
	}
	return payload
}

// emitAudit raft-Applies an AuditAppend command. Failure to emit audit
// is non-fatal — callers shouldn't fail their request because the audit
// store is briefly unavailable.
func (i *Issuer) emitAudit(t pb.AuditEventType, payload map[string]string) error {
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(time.Now()),
		Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
			Event: &pb.AuditEvent{Type: t, Payload: payload},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return i.apply(data)
}

// Handler serves HTTP-01 challenge responses from state.ChallengeTokens.
type Handler struct {
	state *state.State
}

// ServeHTTP responds 200 + plain text key_auth when the token matches a
// live ChallengeToken; 404 otherwise.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, ChallengePath) {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, ChallengePath)
	if token == "" || strings.ContainsRune(token, '/') {
		http.NotFound(w, r)
		return
	}
	ct, ok := h.state.ChallengeTokens.Get(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if ct.GetExpiresAt() != nil && time.Now().After(ct.GetExpiresAt().AsTime()) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(ct.GetKeyAuth()))
}
