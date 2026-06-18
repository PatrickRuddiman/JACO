package rebuild_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/rebuild"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestRebuild_LoadsWhenConfigChanges(t *testing.T) {
	var version atomic.Int64
	build := func() ([]byte, error) {
		v := version.Add(1)
		return []byte(fmt.Sprintf("config-v%d", v)), nil
	}

	loaded := make([][]byte, 0)
	var loadMu sync.Mutex
	load := func(_ context.Context, cfg []byte, _ bool) error {
		loadMu.Lock()
		loaded = append(loaded, append([]byte(nil), cfg...))
		loadMu.Unlock()
		return nil
	}

	r := rebuild.New(watch.NewRegistry(), build, load)
	if err := r.Rebuild(context.Background()); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if err := r.Rebuild(context.Background()); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	if len(loaded) != 2 {
		t.Fatalf("loaded len = %d, want 2", len(loaded))
	}
	if string(loaded[0]) != "config-v1" || string(loaded[1]) != "config-v2" {
		t.Errorf("loaded contents = %v", loaded)
	}
}

func TestRebuild_SkipsLoadWhenConfigIdentical(t *testing.T) {
	build := func() ([]byte, error) { return []byte("static-config"), nil }
	var loadCount atomic.Int64
	load := func(_ context.Context, _ []byte, _ bool) error {
		loadCount.Add(1)
		return nil
	}
	r := rebuild.New(watch.NewRegistry(), build, load)
	for i := 0; i < 5; i++ {
		if err := r.Rebuild(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := loadCount.Load(); got != 1 {
		t.Errorf("load called %d times for identical config; want exactly 1", got)
	}
	if got := r.Rebuilds(); got != 5 {
		t.Errorf("Rebuilds counter = %d, want 5", got)
	}
	if got := r.Loads(); got != 1 {
		t.Errorf("Loads counter = %d, want 1", got)
	}
}

func TestForceReload_LoadsEvenWhenConfigIdentical(t *testing.T) {
	// Regression: a follower that must load a newly-replicated prod leaf calls
	// ForceReload. The rendered config is byte-identical (its automation policy
	// was prod all along), so a plain Rebuild would skip the load and Caddy
	// would never re-run Manage. ForceReload must load anyway, with force=true.
	build := func() ([]byte, error) { return []byte("static-config"), nil }
	var loads, forces atomic.Int64
	load := func(_ context.Context, _ []byte, force bool) error {
		loads.Add(1)
		if force {
			forces.Add(1)
		}
		return nil
	}
	r := rebuild.New(watch.NewRegistry(), build, load)
	if err := r.Rebuild(context.Background()); err != nil { // initial load
		t.Fatalf("initial Rebuild: %v", err)
	}
	if err := r.Rebuild(context.Background()); err != nil { // identical → skipped
		t.Fatalf("identical Rebuild: %v", err)
	}
	if err := r.ForceReload(context.Background()); err != nil { // identical → forced
		t.Fatalf("ForceReload: %v", err)
	}
	if got := loads.Load(); got != 2 {
		t.Errorf("load called %d times; want 2 (initial + forced; the identical Rebuild is skipped)", got)
	}
	if got := forces.Load(); got != 1 {
		t.Errorf("force=true passed %d times; want exactly 1 (the ForceReload)", got)
	}
	if got := r.Loads(); got != 2 {
		t.Errorf("Loads counter = %d, want 2", got)
	}
}

func TestRebuild_PropagatesBuildError(t *testing.T) {
	build := func() ([]byte, error) { return nil, errors.New("build failed") }
	load := func(context.Context, []byte, bool) error { return nil }
	r := rebuild.New(watch.NewRegistry(), build, load)
	err := r.Rebuild(context.Background())
	if err == nil {
		t.Errorf("expected error from Rebuild")
	}
}

func TestRebuild_PropagatesLoadError(t *testing.T) {
	build := func() ([]byte, error) { return []byte("cfg"), nil }
	load := func(context.Context, []byte, bool) error { return errors.New("caddy boom") }
	r := rebuild.New(watch.NewRegistry(), build, load)
	if err := r.Rebuild(context.Background()); err == nil {
		t.Errorf("expected load error")
	}
}

func TestRun_DebouncesBurstsIntoSingleRebuild(t *testing.T) {
	// Fire 5 Route writes in quick succession; expect 1 rebuild after the
	// debounce window (plus 1 initial pass at startup = 2 total).
	brokers := watch.NewRegistry()
	st := state.New(brokers)

	var rebuilds atomic.Int64
	build := func() ([]byte, error) {
		rebuilds.Add(1)
		return []byte(fmt.Sprintf("v%d", rebuilds.Load())), nil
	}
	load := func(context.Context, []byte, bool) error { return nil }
	r := rebuild.New(brokers, build, load)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Run(ctx) }()

	// Wait for the initial pass to complete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rebuilds.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Burst 5 route writes.
	for i := 0; i < 5; i++ {
		st.Routes.Apply(&pb.Route{Domain: fmt.Sprintf("d%d.example.com", i), Deployment: "x", Service: "y"}, uint64(100+i))
		time.Sleep(10 * time.Millisecond)
	}

	// Wait for debounce window.
	time.Sleep(rebuild.DebounceWindow + 200*time.Millisecond)

	got := rebuilds.Load()
	// Expected: 1 initial + 1 debounced = 2 (the 5 events collapse into one
	// rebuild). Allow a +1 slack in case a second debounced pass fired
	// because state landed during the rebuild.
	if got < 2 || got > 3 {
		t.Errorf("rebuilds after 5 events = %d, want 2 (or up to 3 with slack)", got)
	}
}

func TestRun_TCPRouteEventTriggersRebuild(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)

	var rebuilds atomic.Int64
	build := func() ([]byte, error) {
		rebuilds.Add(1)
		return []byte(fmt.Sprintf("v%d", rebuilds.Load())), nil
	}
	load := func(context.Context, []byte, bool) error { return nil }
	r := rebuild.New(brokers, build, load)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let the initial pass complete

	before := rebuilds.Load()
	st.TCPRoutes.Apply(&pb.TCPRoute{PublishedPort: 5432, Deployment: "data", Service: "db", ContainerPort: 5432}, 1)
	time.Sleep(rebuild.DebounceWindow + 150*time.Millisecond)

	if got := rebuilds.Load(); got != before+1 {
		t.Errorf("rebuilds after TCPRoute event = %d, want %d (one debounced rebuild)", got, before+1)
	}
}

func TestRun_HandlesRoutesObservedCertsTokens(t *testing.T) {
	// Verify each subscribed broker triggers a rebuild.
	brokers := watch.NewRegistry()
	st := state.New(brokers)

	var rebuilds atomic.Int64
	build := func() ([]byte, error) {
		rebuilds.Add(1)
		return []byte(fmt.Sprintf("v%d", rebuilds.Load())), nil
	}
	load := func(context.Context, []byte, bool) error { return nil }
	r := rebuild.New(brokers, build, load)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // initial pass

	// Each broker write -> one debounced rebuild.
	st.Routes.Apply(&pb.Route{Domain: "a.example.com", Deployment: "x", Service: "y"}, 1)
	time.Sleep(rebuild.DebounceWindow + 100*time.Millisecond)
	st.ReplicasObserved.Apply(&pb.ReplicaObserved{Id: "r1", State: pb.ReplicaState_REPLICA_STATE_RUNNING}, 2)
	time.Sleep(rebuild.DebounceWindow + 100*time.Millisecond)
	st.Certs.Apply(&pb.Cert{Domain: "a.example.com"}, 3)
	time.Sleep(rebuild.DebounceWindow + 100*time.Millisecond)
	st.ChallengeTokens.Apply(&pb.ChallengeToken{Token: "t1"}, 4)
	time.Sleep(rebuild.DebounceWindow + 100*time.Millisecond)
	st.CertBlobs.Apply(&pb.CertBlob{Key: "certificates/staging/a.example.com/a.example.com.crt"}, 5)
	time.Sleep(rebuild.DebounceWindow + 100*time.Millisecond)

	got := rebuilds.Load()
	// 1 initial + 5 events = 6. Slack +1 for any debounce racing during the
	// initial pass.
	if got < 6 || got > 7 {
		t.Errorf("rebuilds = %d, want 6 (one per broker after initial)", got)
	}
}
