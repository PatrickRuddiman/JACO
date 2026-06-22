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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/logging"
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

	// Logger logs cert lifecycle audit-emit failures at WARN (emitting audit
	// is non-fatal, so the apply path swallows the error — but an operator
	// still wants to know the audit trail has a hole). nil → discard. Set by
	// the daemon after construction; tests leave it nil.
	Logger *slog.Logger
}

func (i *Issuer) log() *slog.Logger {
	if i.Logger == nil {
		return logging.Discard()
	}
	return i.Logger
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
func (i *Issuer) Issue(ctx context.Context, domain, token, keyAuth string) error {
	if applyErr := i.PublishToken(ctx, domain, token, keyAuth); applyErr != nil {
		if auditErr := i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, i.withEnv(map[string]string{
			"domain": domain,
			"reason": applyErr.Error(),
		})); auditErr != nil {
			i.log().Warn("cert audit emit failed",
				"event", "certificate_failed", "domain", domain, "error", auditErr)
		}
		return applyErr
	}
	if auditErr := i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED, i.withEnv(map[string]string{
		"domain": domain,
	})); auditErr != nil {
		i.log().Warn("cert audit emit failed",
			"event", "certificate_renewed", "domain", domain, "error", auditErr)
	}
	return nil
}

// PublishToken raft-Applies a ChallengeTokenStore WITHOUT emitting any audit
// event. It is the cluster-distribution primitive behind the embedded
// CertMagic path: the storage tap (internal/ingress/storage) calls it for
// every challenge_tokens blob CertMagic writes, so the token reaches
// state.ChallengeTokens on every node and any peer can serve the HTTP-01
// validation. Audit emission is deliberately omitted — CertMagic writes one
// blob per order attempt, and a CERTIFICATE_RENEWED per write would spam the
// audit log. Use Issue when the single, intentional audit pair is wanted.
func (i *Issuer) PublishToken(_ context.Context, domain, token, keyAuth string) error {
	if domain == "" || token == "" || keyAuth == "" {
		return fmt.Errorf("PublishToken: domain + token + keyAuth required")
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
	return i.apply(data)
}

// ParseHTTP01Blob extracts the (domain, token, keyAuth) triple from the JSON
// CertMagic stores under its `.../challenge_tokens/<domain>.json` storage key
// (a marshaled acmez acme.Challenge — see certmagic distributedSolver.Present).
// ok is false for any non-http-01 challenge or a blob missing the token /
// key-authorization / identifier, so the storage tap can ignore tls-alpn-01
// and dns-01 writes. A local struct is used so the challenge package need not
// import acmez; the JSON tags are the stable RFC 8555 field names.
func ParseHTTP01Blob(value []byte) (domain, token, keyAuth string, ok bool) {
	var chal struct {
		Type             string `json:"type"`
		Token            string `json:"token"`
		KeyAuthorization string `json:"keyAuthorization"`
		Identifier       struct {
			Value string `json:"value"`
		} `json:"identifier"`
	}
	if err := json.Unmarshal(value, &chal); err != nil {
		return "", "", "", false
	}
	if chal.Type != "http-01" || chal.Token == "" || chal.KeyAuthorization == "" || chal.Identifier.Value == "" {
		return "", "", "", false
	}
	return chal.Identifier.Value, chal.Token, chal.KeyAuthorization, true
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
	if auditErr := i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, map[string]string{
		"domain":           domain,
		"acme_environment": EnvStaging,
		"stage_failed_at":  "staging",
		"failure_class":    string(ClassifyFailure(err)),
		"reason":           err.Error(),
	}); auditErr != nil {
		i.log().Warn("cert audit emit failed",
			"event", "certificate_failed", "domain", domain, "error", auditErr)
	}
}

// EmitProdFailure records a CERTIFICATE_FAILED audit event for a failed prod
// ACME issuance (PendingProdWindow expired without a cert landing). Stamps
// failure_class=rate_limit and retry_after so operators can correlate with LE's
// failed-auth rate-limit window. Issue #189.
func (i *Issuer) EmitProdFailure(domain string, retryAfter time.Duration) {
	if auditErr := i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, map[string]string{
		"domain":           domain,
		"acme_environment": EnvProd,
		"failure_class":    string(FailureRateLimit),
		"retry_after":      retryAfter.String(),
	}); auditErr != nil {
		i.log().Warn("cert audit emit failed",
			"event", "certificate_failed", "domain", domain, "error", auditErr)
	}
}

// EmitIssued records a CERTIFICATE_ISSUED audit event tagged with the
// environment the cert was issued against.
func (i *Issuer) EmitIssued(domain, env string) {
	if auditErr := i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_ISSUED, map[string]string{
		"domain":           domain,
		"acme_environment": env,
	}); auditErr != nil {
		i.log().Warn("cert audit emit failed",
			"event", "certificate_issued", "domain", domain, "error", auditErr)
	}
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

// NewHandler constructs a Handler that serves HTTP-01 key authorizations from
// st.ChallengeTokens. The daemon shares one *state.State across every
// subsystem, so a Handler built here serves any token replicated into raft —
// including ones an order-initiating peer published moments ago.
func NewHandler(st *state.State) *Handler { return &Handler{state: st} }

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
