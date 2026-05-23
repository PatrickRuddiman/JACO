package storage

import (
	"github.com/caddyserver/caddy/v2"
)

// CaddyModule registers *JacoStorage as the "caddy.storage.jaco" module
// so a Caddy config carrying `"storage": {"module": "jaco"}` resolves to
// our raft-backed implementation (task 33 deferral).
//
// Caddy's module loader needs a zero-value New() factory — JacoStorage
// can't be zero-instantiated because it needs state + apply +
// hostname injected by the daemon. The daemon constructs a real
// JacoStorage at startup and assigns it to defaultStorage via
// SetDefaultStorage; CaddyModule then hands that pointer back from
// New(). Configs that reach New() without SetDefaultStorage having
// fired get a nil pointer and a clear error in Provision.
func (s *JacoStorage) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.storage.jaco",
		New: func() caddy.Module { return defaultStorage },
	}
}

// defaultStorage is the singleton the daemon constructs and shares with
// Caddy's module loader. SetDefaultStorage assigns it; CaddyModule
// returns it. Reset to nil at daemon shutdown so subsequent test
// invocations don't pick up a stale pointer.
var defaultStorage *JacoStorage

// SetDefaultStorage hands the daemon-built JacoStorage to the caddy
// module registry. Idempotent; safe to call multiple times.
func SetDefaultStorage(s *JacoStorage) { defaultStorage = s }

func init() {
	caddy.RegisterModule(&JacoStorage{})
}
