package storage_test

import (
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
)

// TestSetDefaultStorage_IdempotentAndExposedViaCaddyModule — Caddy's
// module loader calls New() to get a fresh instance; the daemon
// stashes the real *JacoStorage in defaultStorage so the loader hands
// it back. We exercise that round-trip.
func TestSetDefaultStorage_IdempotentAndExposedViaCaddyModule(t *testing.T) {
	st := state.New(watch.NewRegistry())
	s := storage.New(st, func([]byte) error { return nil }, "host-a", nil)

	storage.SetDefaultStorage(s)
	// Second call must be safe (idempotent).
	storage.SetDefaultStorage(s)

	info := s.CaddyModule()
	if info.ID != "caddy.storage.jaco" {
		t.Errorf("ModuleInfo.ID = %q, want caddy.storage.jaco", info.ID)
	}
	mod := info.New()
	if mod == nil {
		t.Errorf("New() returned nil")
	}
	if got, ok := mod.(*storage.JacoStorage); !ok || got != s {
		t.Errorf("New() did not return the SetDefaultStorage pointer; got %T %v", mod, got)
	}

	// Reset to nil for any subsequent tests (Caddy's global registry
	// would otherwise see a stale pointer).
	storage.SetDefaultStorage(nil)
}
