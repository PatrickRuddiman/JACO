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

// Clock abstracts time.Now so tests can pin expiry.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns the production Clock.
func SystemClock() Clock { return systemClock{} }

// Issuer publishes ChallengeTokens to raft.
type Issuer struct {
	apply Applier
	clock Clock
}

// NewIssuer constructs an Issuer. clock nil → SystemClock.
func NewIssuer(apply Applier, clock Clock) *Issuer {
	if clock == nil {
		clock = SystemClock()
	}
	return &Issuer{apply: apply, clock: clock}
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
	expiresAt := i.clock.Now().Add(TokenTTL)
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(i.clock.Now()),
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
		_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_FAILED, map[string]string{
			"domain": domain,
			"reason": applyErr.Error(),
		})
		return applyErr
	}
	_ = i.emitAudit(pb.AuditEventType_AUDIT_EVENT_TYPE_CERTIFICATE_RENEWED, map[string]string{
		"domain": domain,
	})
	return nil
}

// emitAudit raft-Applies an AuditAppend command. Failure to emit audit
// is non-fatal — callers shouldn't fail their request because the audit
// store is briefly unavailable.
func (i *Issuer) emitAudit(t pb.AuditEventType, payload map[string]string) error {
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(i.clock.Now()),
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
	clock Clock
}

// NewHandler constructs a Handler. clock nil → SystemClock.
func NewHandler(st *state.State, clock Clock) *Handler {
	if clock == nil {
		clock = SystemClock()
	}
	return &Handler{state: st, clock: clock}
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
	if ct.GetExpiresAt() != nil && h.clock.Now().After(ct.GetExpiresAt().AsTime()) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(ct.GetKeyAuth()))
}
