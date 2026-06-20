package challenge

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
)

// CaddyHandler registers as the "http.handlers.jaco_acme_challenge" Caddy
// module so a config carrying `{"handler": "jaco_acme_challenge"}` resolves to
// JACO's raft-backed HTTP-01 responder. It exists because CertMagic's own
// distributed HTTP-01 solver keys its storage by issuer (CA), so a follower
// rendering a different CA policy than the order-initiating leader — which
// happens during the stage-first staging window (issue #182) and again at
// prod promotion — reads the wrong storage prefix, misses the token, and 404s
// the validation (issue #189). This handler is CA-agnostic: it serves the
// key authorization keyed purely by the challenge token from
// state.ChallengeTokens, which every node replicates, so any node behind an
// L4 load balancer can answer the CA's validation regardless of which CA
// policy it currently renders.
//
// Prepended ahead of the user routes on the :80 server, it is terminal for
// `/.well-known/acme-challenge/*`: it never proxies a challenge path to a
// backend, returning 404 on an unknown / expired token so the CA retries.
type CaddyHandler struct{}

// CaddyModule satisfies caddy.Module. Like the storage module, the real
// state comes from the daemon via SetDefaultChallengeState before Caddy
// starts serving; New() hands back a zero-value handler that reads the
// shared singleton at request time.
func (CaddyHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.jaco_acme_challenge",
		New: func() caddy.Module { return new(CaddyHandler) },
	}
}

// ServeHTTP serves the HTTP-01 key authorization for the requested token, or
// 404 when the token is unknown / expired. It is terminal: next is never
// called, so a challenge path can never fall through to a proxied backend.
func (CaddyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	st := defaultChallengeState
	if st == nil {
		http.NotFound(w, r)
		return nil
	}
	NewHandler(st).ServeHTTP(w, r)
	return nil
}

// defaultChallengeState is the singleton *state.State the daemon shares with
// Caddy's module loader. SetDefaultChallengeState assigns it; CaddyHandler
// reads it per request. Reset to nil at daemon shutdown so a later test
// invocation doesn't pick up a stale pointer.
var defaultChallengeState *state.State

// SetDefaultChallengeState hands the daemon-built *state.State to the
// jaco_acme_challenge module. Idempotent; safe to call multiple times.
func SetDefaultChallengeState(st *state.State) { defaultChallengeState = st }

func init() {
	caddy.RegisterModule(CaddyHandler{})
}
