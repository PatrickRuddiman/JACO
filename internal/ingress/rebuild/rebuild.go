// Package rebuild owns the debounced Caddy config-reload loop. Watches
// Routes / ReplicaObserved / Certs / ChallengeTokens; on any event,
// debounces for DebounceWindow, then re-renders via the injected builder
// and calls the injected loader unless the new bytes are identical to the
// previously-loaded config (saves a Caddy reload roundtrip).
package rebuild

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

// DebounceWindow batches bursts of watch events into one rebuild pass.
const DebounceWindow = 200 * time.Millisecond

// Builder rebuilds the Caddy config from current state. The daemon wires
// this to config.BuildCaddyConfig.
type Builder func() ([]byte, error)

// Loader hands the rebuilt config to Caddy. The daemon wires this to
// `caddy.Load(cfg, false)`.
type Loader func(ctx context.Context, cfg []byte) error

// Reloader is the goroutine that ties watch events to caddy.Load.
type Reloader struct {
	brokers *watch.Registry
	build   Builder
	load    Loader

	// rebuildMu serializes the whole build→compare→load→store sequence so
	// concurrent Rebuild callers (the watch loop AND the stage-first
	// reconcile loop, issue #41) don't race on caddy.Load or interleave the
	// lastCfg TOCTOU. mu (below) only guards lastCfg for the stats readers.
	rebuildMu sync.Mutex

	mu      sync.Mutex
	lastCfg []byte

	// Stats — exposed for tests to assert.
	rebuilds atomic.Int64
	loads    atomic.Int64
}

// New constructs a Reloader.
func New(brokers *watch.Registry, build Builder, load Loader) *Reloader {
	return &Reloader{brokers: brokers, build: build, load: load}
}

// Rebuild forces an immediate rebuild + load pass. Returns nil when the
// new config is byte-identical to the previously-loaded one (no load
// issued) or when the load completes; returns the build / load error
// otherwise.
func (r *Reloader) Rebuild(ctx context.Context) error {
	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()
	r.rebuilds.Add(1)
	cfg, err := r.build()
	if err != nil {
		return fmt.Errorf("build config: %w", err)
	}
	r.mu.Lock()
	identical := bytes.Equal(cfg, r.lastCfg)
	r.mu.Unlock()
	if identical {
		return nil
	}
	if err := r.load(ctx, cfg); err != nil {
		return fmt.Errorf("caddy load: %w", err)
	}
	r.mu.Lock()
	r.lastCfg = cfg
	r.mu.Unlock()
	r.loads.Add(1)
	return nil
}

// Run drives the debounced loop until ctx cancellation. The initial pass
// fires immediately so the daemon's first config lands on startup.
func (r *Reloader) Run(ctx context.Context) error {
	routes := r.brokers.Routes.Subscribe()
	defer routes.Cancel()
	obs := r.brokers.ReplicasObserved.Subscribe()
	defer obs.Cancel()
	certs := r.brokers.Certs.Subscribe()
	defer certs.Cancel()
	tokens := r.brokers.ChallengeTokens.Subscribe()
	defer tokens.Cancel()

	_ = r.Rebuild(ctx) // initial load

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pending := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-routes.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-obs.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-certs.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-tokens.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-debounce.C:
			if pending {
				pending = false
				_ = r.Rebuild(ctx)
			}
		}
	}
}

// Rebuilds returns how many times Rebuild has been called (test seam).
func (r *Reloader) Rebuilds() int64 { return r.rebuilds.Load() }

// Loads returns how many times the loader successfully ran (test seam).
func (r *Reloader) Loads() int64 { return r.loads.Load() }

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

