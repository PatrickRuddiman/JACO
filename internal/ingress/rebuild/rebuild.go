// Package rebuild owns the debounced Caddy config-reload loop. Watches
// Routes / TCPRoutes / ReplicaObserved / Certs / CertBlobs / ChallengeTokens;
// on any event, debounces for DebounceWindow, then re-renders via the injected
// builder and calls the injected loader unless the new bytes are identical to
// the previously-loaded config (saves a Caddy reload roundtrip).
package rebuild

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/logging"
)

// DebounceWindow batches bursts of watch events into one rebuild pass.
const DebounceWindow = 200 * time.Millisecond

// Builder rebuilds the Caddy config from current state. The daemon wires
// this to config.BuildCaddyConfig.
type Builder func() ([]byte, error)

// Loader hands the rebuilt config to Caddy. The daemon wires this to
// `caddy.Load(cfg, force)`. When force is true Caddy re-provisions even if the
// config is byte-identical to what's running, so certmagic re-runs Manage and
// loads any cert that appeared in storage since the last load — the path a
// follower needs to pick up a newly-replicated prod leaf (see ForceReload).
type Loader func(ctx context.Context, cfg []byte, force bool) error

// Reloader is the goroutine that ties watch events to caddy.Load.
type Reloader struct {
	brokers *watch.Registry
	build   Builder
	load    Loader

	// Logger is the ingress subsystem logger. nil → a discard logger.
	Logger *slog.Logger

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

// log returns the configured logger, or a discard logger when none was set
// (e.g. tests that construct a bare Reloader).
func (r *Reloader) log() *slog.Logger {
	if r.Logger == nil {
		return logging.Discard()
	}
	return r.Logger
}

// Rebuild forces an immediate rebuild + load pass. Returns nil when the
// new config is byte-identical to the previously-loaded one (no load
// issued) or when the load completes; returns the build / load error
// otherwise.
func (r *Reloader) Rebuild(ctx context.Context) error {
	return r.rebuild(ctx, false)
}

// ForceReload rebuilds and loads UNCONDITIONALLY, bypassing the byte-identical
// short-circuit and asking Caddy to re-provision even when the rendered config
// hasn't changed. It exists for the one effect the rendered config can't
// express: a follower must re-run certmagic's Manage to load a
// newly-replicated prod leaf into Caddy's in-memory cache, but because the
// leader (not the follower) obtained that cert, the follower's automation
// policy — and thus its rendered config — is byte-identical before and after
// the leaf lands. Without a forced reload Manage never re-runs and the follower
// serves no TLS until a daemon restart.
func (r *Reloader) ForceReload(ctx context.Context) error {
	return r.rebuild(ctx, true)
}

// rebuild is the shared build→compare→load→store core. When force is false it
// skips the load if the new config is byte-identical to the last-loaded one;
// when force is true it always loads and passes force through to the Loader so
// caddy.Load re-provisions even on an identical config.
func (r *Reloader) rebuild(ctx context.Context, force bool) error {
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
	if identical && !force {
		return nil
	}
	if err := r.load(ctx, cfg, force); err != nil {
		r.log().Error("caddy config reload failed", "bytes", len(cfg), "force", force, "error", err)
		return fmt.Errorf("caddy load: %w", err)
	}
	r.mu.Lock()
	r.lastCfg = cfg
	r.mu.Unlock()
	r.loads.Add(1)
	r.log().Info("caddy config reloaded", "bytes", len(cfg), "force", force)
	return nil
}

// Run drives the debounced loop until ctx cancellation. The initial pass
// fires immediately so the daemon's first config lands on startup.
func (r *Reloader) Run(ctx context.Context) error {
	routes := r.brokers.Routes.Subscribe()
	defer routes.Cancel()
	tcpRoutes := r.brokers.TCPRoutes.Subscribe()
	defer tcpRoutes.Cancel()
	obs := r.brokers.ReplicasObserved.Subscribe()
	defer obs.Cancel()
	certs := r.brokers.Certs.Subscribe()
	defer certs.Cancel()
	// CertBlobs drives a re-render so a follower flips its automation policy
	// (staging→prod) the moment a promotion replicates: the staging-vs-prod
	// policy is derived from replicated cert-blob state on non-leader nodes
	// (issue #182). Without this the follower would keep rendering the stale
	// policy until some other watched store happened to change.
	certBlobs := r.brokers.CertBlobs.Subscribe()
	defer certBlobs.Cancel()
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
		case <-tcpRoutes.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-obs.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-certs.Events():
			pending = true
			resetTimer(debounce, DebounceWindow)
		case <-certBlobs.Events():
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
