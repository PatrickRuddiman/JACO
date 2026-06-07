package health_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// ---- fakes -----------------------------------------------------------------

// fakeDocker only implements ContainerInspect — Watcher needs nothing else.
// Other methods on dockerx.Docker panic via the embedded interface, which is
// the noisy-on-misuse contract we want.
type fakeDocker struct {
	dockerx.Docker
	mu         sync.Mutex
	state      *types.ContainerState
	inspectErr error
	networks   map[string]*network.EndpointSettings
}

func (f *fakeDocker) ContainerInspect(_ context.Context, _ string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return types.ContainerJSON{}, f.inspectErr
	}
	cj := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:    "c-1",
			State: f.state,
		},
	}
	if f.networks != nil {
		cj.NetworkSettings = &types.NetworkSettings{Networks: f.networks}
	}
	return cj, nil
}

func (f *fakeDocker) setState(s *types.ContainerState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakeDocker) setInspectErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectErr = err
}

// fakeClock blocks on After() until the test calls Advance(d). Each pending
// After receives a separate channel; Advance fires the ones whose duration
// matches the call.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*pendingAfter
}

type pendingAfter struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.pending = append(c.pending, &pendingAfter{d: d, ch: ch})
	c.mu.Unlock()
	return ch
}

// Advance fires every pending After with d <= delta, in order, advancing the
// clock to the fire time. Returns the number of timers fired.
func (c *fakeClock) Advance(delta time.Duration) int {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	var fired []*pendingAfter
	var remaining []*pendingAfter
	for _, p := range c.pending {
		if p.d <= delta {
			fired = append(fired, p)
		} else {
			remaining = append(remaining, p)
		}
	}
	c.pending = remaining
	c.mu.Unlock()
	for _, p := range fired {
		p.ch <- c.now
	}
	return len(fired)
}

// pendingCount returns how many After() calls are waiting to be advanced.
// Useful for synchronizing tests with the watcher goroutine.
func (c *fakeClock) pendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// waitForPending spins until at least n After() calls are queued. Bounded
// to avoid hangs.
func (c *fakeClock) waitForPending(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.pendingCount() >= n {
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
	t.Fatalf("waitForPending(%d) timed out; pending=%d", n, c.pendingCount())
}

// recordingSubmit captures every ReplicaObserved the Watcher emits.
type recordingSubmit struct {
	mu       sync.Mutex
	observed []*pb.ReplicaObserved
	ch       chan struct{}
}

func newRecordingSubmit() *recordingSubmit {
	return &recordingSubmit{ch: make(chan struct{}, 256)}
}

func (r *recordingSubmit) Submit(_ context.Context, obs *pb.ReplicaObserved) error {
	r.mu.Lock()
	r.observed = append(r.observed, obs)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
	return nil
}

func (r *recordingSubmit) snapshot() []*pb.ReplicaObserved {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*pb.ReplicaObserved, len(r.observed))
	copy(out, r.observed)
	return out
}

func (r *recordingSubmit) waitForCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.observed)
		r.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("waitForCalls(%d): only %d arrived", n, len(r.observed))
}

// silence unused imports
var (
	_ = container.Config{}
	_ = network.NetworkingConfig{}
	_ = ocispec.Platform{}
	_ = fmt.Sprintf
)

// ---- Tests -----------------------------------------------------------------

func TestWatcher_HealthcheckStartingToHealthyTransitions(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{
		Status: "running",
		Health: &types.Health{Status: "starting"},
	}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)

	w.Start(context.Background(), "sample-web-0", "c-1", true)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	// Tick 1: starting → pending.
	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)
	if got := sub.snapshot()[0].GetState(); got != pb.ReplicaState_REPLICA_STATE_PENDING {
		t.Errorf("tick 1 state = %v, want PENDING", got)
	}

	// Flip docker to healthy.
	d.setState(&types.ContainerState{Status: "running", Health: &types.Health{Status: "healthy"}})

	// Tick 2: healthy → running.
	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 2)
	obs := sub.snapshot()[1]
	if obs.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
		t.Errorf("tick 2 state = %v, want RUNNING", obs.GetState())
	}
	if obs.GetLastHealthAt() == nil {
		t.Errorf("LastHealthAt nil after RUNNING")
	}
	if obs.GetStartedAt() == nil {
		t.Errorf("StartedAt nil after RUNNING")
	}
	if obs.GetContainerId() != "c-1" {
		t.Errorf("ContainerId = %q", obs.GetContainerId())
	}
}

func TestWatcher_HealthcheckUnhealthyEmitsDegraded(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{
		Status: "running",
		Health: &types.Health{Status: "unhealthy"},
	}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", true)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	if got := sub.snapshot()[0].GetState(); got != pb.ReplicaState_REPLICA_STATE_DEGRADED {
		t.Errorf("state = %v, want DEGRADED", got)
	}
}

func TestWatcher_NoHealthcheckRequiresFiveConsecutiveRunningPolls(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{Status: "running"}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", false /* no healthcheck */)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	// First four polls — still PENDING (counter ramping).
	for i := 0; i < 4; i++ {
		clock.waitForPending(t, 1)
		clock.Advance(health.FastPollInterval)
		sub.waitForCalls(t, i+1)
		if got := sub.snapshot()[i].GetState(); got != pb.ReplicaState_REPLICA_STATE_PENDING {
			t.Errorf("tick %d state = %v, want PENDING", i+1, got)
		}
	}
	// Fifth poll — RUNNING.
	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 5)
	if got := sub.snapshot()[4].GetState(); got != pb.ReplicaState_REPLICA_STATE_RUNNING {
		t.Errorf("tick 5 state = %v, want RUNNING (5 consecutive running polls)", got)
	}
}

func TestWatcher_ExitedContainerEmitsFailedWithExitCode(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{Status: "exited", ExitCode: 137}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", false)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	obs := sub.snapshot()[0]
	if obs.GetState() != pb.ReplicaState_REPLICA_STATE_FAILED {
		t.Errorf("state = %v, want FAILED", obs.GetState())
	}
	if obs.GetCode() != "container_exited" {
		t.Errorf("code = %q, want container_exited", obs.GetCode())
	}
	if got := obs.GetDetails()["exit_code"]; got != "137" {
		t.Errorf("details.exit_code = %q, want 137", got)
	}
}

// TestWatcher_PopulatesPerNetworkIPs — poll records each attached network's
// container IP under Details["ip.<dockerNetwork>"] (issue #28), so the DNS
// responder can resolve service names to per-host IPs.
func TestWatcher_PopulatesPerNetworkIPs(t *testing.T) {
	d := &fakeDocker{
		state: &types.ContainerState{Status: "running"},
		networks: map[string]*network.EndpointSettings{
			"jaco_app__default": {IPAddress: "10.244.5.2"},
			"jaco_app_backend":  {IPAddress: "10.244.6.2"},
		},
	}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "app-web-0", "c-1", false)
	t.Cleanup(func() { w.Stop("app-web-0") })

	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	obs := sub.snapshot()[0]
	if got := obs.GetDetails()["ip.jaco_app__default"]; got != "10.244.5.2" {
		t.Errorf("ip.jaco_app__default = %q, want 10.244.5.2", got)
	}
	if got := obs.GetDetails()["ip.jaco_app_backend"]; got != "10.244.6.2" {
		t.Errorf("ip.jaco_app_backend = %q, want 10.244.6.2", got)
	}
}

func TestWatcher_InspectErrorEmitsFailedInspectFailed(t *testing.T) {
	d := &fakeDocker{}
	d.setInspectErr(fmt.Errorf("docker is down"))
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", true)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	obs := sub.snapshot()[0]
	if obs.GetState() != pb.ReplicaState_REPLICA_STATE_FAILED {
		t.Errorf("state = %v, want FAILED", obs.GetState())
	}
	if obs.GetCode() != "inspect_failed" {
		t.Errorf("code = %q, want inspect_failed", obs.GetCode())
	}
}

func TestWatcher_LastHealthAtIsAlwaysFresh(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{
		Status: "running",
		Health: &types.Health{Status: "healthy"},
	}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", true)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	clock.waitForPending(t, 1)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	obs := sub.snapshot()[0]
	if obs.GetLastHealthAt() == nil {
		t.Fatalf("LastHealthAt nil")
	}
	// The AC: last_health_at is set and corresponds to the fake clock's
	// current time (which advanced when we called Advance).
	got := obs.GetLastHealthAt().AsTime()
	if got.IsZero() {
		t.Errorf("LastHealthAt is zero")
	}
	if delta := clock.Now().Sub(got); delta > 0 && delta > 100*time.Millisecond {
		t.Errorf("LastHealthAt drift = %v (want < 100ms)", delta)
	}
}

func TestWatcher_StateMappingIsClosed_OnlyKnownEnumValuesEmitted(t *testing.T) {
	// Run several scenarios and assert every emitted state is in the closed
	// ReplicaState set — never an unspecified, updating, pulling, or stopped
	// value (the watcher doesn't emit those; they're owned by other
	// subsystems).
	closed := map[pb.ReplicaState]bool{
		pb.ReplicaState_REPLICA_STATE_PENDING:  true,
		pb.ReplicaState_REPLICA_STATE_RUNNING:  true,
		pb.ReplicaState_REPLICA_STATE_DEGRADED: true,
		pb.ReplicaState_REPLICA_STATE_FAILED:   true,
	}

	scenarios := []*types.ContainerState{
		{Status: "running", Health: &types.Health{Status: "starting"}},
		{Status: "running", Health: &types.Health{Status: "healthy"}},
		{Status: "running", Health: &types.Health{Status: "unhealthy"}},
		{Status: "exited", ExitCode: 1},
		{Status: "created"},
	}
	for i, s := range scenarios {
		d := &fakeDocker{state: s}
		sub := newRecordingSubmit()
		clock := newFakeClock()
		w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
		w.Start(context.Background(), fmt.Sprintf("r-%d", i), "c-1", true)
		clock.waitForPending(t, 1)
		clock.Advance(health.FastPollInterval)
		sub.waitForCalls(t, 1)
		got := sub.snapshot()[0].GetState()
		if !closed[got] {
			t.Errorf("scenario %d emitted unexpected state %v", i, got)
		}
		w.StopAll()
	}
}

func TestWatcher_StopCancelsLoop(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{Status: "running"}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", false)
	clock.waitForPending(t, 1)

	if got := w.Active(); got != 1 {
		t.Errorf("Active = %d, want 1", got)
	}
	w.Stop("sample-web-0")
	// After cancel the loop returns; the watcher map is empty.
	if got := w.Active(); got != 0 {
		t.Errorf("Active after Stop = %d, want 0", got)
	}
}

func TestWatcher_PollCadenceSwitchesToSlowAfterRunning(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{
		Status: "running",
		Health: &types.Health{Status: "healthy"},
	}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	w.Start(context.Background(), "sample-web-0", "c-1", true)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	// First tick at FastPoll while state is PENDING.
	clock.waitForPending(t, 1)
	firstDelay := clock.peekFirstDelay()
	if firstDelay != health.FastPollInterval {
		t.Errorf("first interval = %v, want %v", firstDelay, health.FastPollInterval)
	}
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 1)

	// State is now RUNNING; the next select should wait SlowPollInterval.
	clock.waitForPending(t, 1)
	if d := clock.peekFirstDelay(); d != health.SlowPollInterval {
		t.Errorf("second interval = %v, want %v", d, health.SlowPollInterval)
	}
}

// peekFirstDelay returns the delay of the oldest pending After() without
// firing it. Used to assert poll cadence.
func (c *fakeClock) peekFirstDelay() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return 0
	}
	return c.pending[0].d
}

// TestWatcher_StartIdempotentOnSameContainer_PreservesCounter pins the fix
// for issue #152. Pre-fix, every reconciler.runStart call invoked
// Watcher.Start unconditionally, and Start unconditionally Stop+recreated
// the per-replica watcher with consecutiveRunning = 0. In a stack with
// stuck depends_on waiters the reconciler re-dispatches faster than 5
// polls, so the no-healthcheck fallback path (classify, line 285-289)
// never accumulated HealthyConsecutiveCount and healthcheck-less replicas
// stayed PENDING indefinitely.
//
// Post-fix: Start is a no-op when called with the same (replicaID,
// containerID), so the existing goroutine keeps polling and the counter
// reaches 5 across re-dispatches.
func TestWatcher_StartIdempotentOnSameContainer_PreservesCounter(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{Status: "running"}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	ctx := context.Background()
	w.Start(ctx, "sample-web-0", "c-1", false /* no healthcheck */)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	// 3 ramping polls; counter at 3, still PENDING.
	for i := 0; i < 3; i++ {
		clock.waitForPending(t, 1)
		clock.Advance(health.FastPollInterval)
		sub.waitForCalls(t, i+1)
		if got := sub.snapshot()[i].GetState(); got != pb.ReplicaState_REPLICA_STATE_PENDING {
			t.Fatalf("ramp tick %d state = %v, want PENDING", i+1, got)
		}
	}

	// Reconciler re-dispatches the same replica (depends_on safety tick,
	// sibling state change, broker resync — any of the dozens of paths
	// that funnel through runStart). Container is unchanged. Pre-fix this
	// would tear down the watcher and reset the counter to 0. Post-fix:
	// no-op, counter stays at 3.
	w.Start(ctx, "sample-web-0", "c-1", false)
	w.Start(ctx, "sample-web-0", "c-1", false)
	w.Start(ctx, "sample-web-0", "c-1", false)

	// Two more polls finish the transition. If the counter had been reset,
	// these two would put it at 2 and the replica would still be PENDING.
	for i := 0; i < 2; i++ {
		clock.waitForPending(t, 1)
		clock.Advance(health.FastPollInterval)
		sub.waitForCalls(t, 4+i)
	}
	final := sub.snapshot()[len(sub.snapshot())-1]
	if final.GetState() != pb.ReplicaState_REPLICA_STATE_RUNNING {
		t.Errorf("after 3 ramp + 3 redispatch + 2 final polls: state = %v, want RUNNING (#152 regression — counter was reset across re-dispatches)",
			final.GetState())
	}
}

// TestWatcher_StartOnNewContainer_ResetsCounter pins the other half of the
// contract: when the containerID actually changes (a real recreate — e.g.
// the scheduler's #148 spec_hash drift detector fired and lifecycle.Start
// stop+removed+created a fresh container), the watcher MUST tear down and
// start fresh. The old container's poll history is meaningless for the
// new one.
func TestWatcher_StartOnNewContainer_ResetsCounter(t *testing.T) {
	d := &fakeDocker{state: &types.ContainerState{Status: "running"}}
	sub := newRecordingSubmit()
	clock := newFakeClock()
	w := health.NewWatcher(d, sub.Submit, clock.Now, clock.After)
	ctx := context.Background()
	w.Start(ctx, "sample-web-0", "c-1", false)
	t.Cleanup(func() { w.Stop("sample-web-0") })

	// 4 ramping polls; counter at 4.
	for i := 0; i < 4; i++ {
		clock.waitForPending(t, 1)
		clock.Advance(health.FastPollInterval)
		sub.waitForCalls(t, i+1)
	}
	// Recreate: new container ID. Counter must reset for c-2.
	w.Start(ctx, "sample-web-0", "c-2", false)

	// Goroutine A's leftover After (queued before Stop cancelled it) plus
	// Goroutine B's first After give pending=2. Drain both — A's send
	// reaches a dead channel (no submit), B polls and submits the 5th
	// observation.
	clock.waitForPending(t, 2)
	clock.Advance(health.FastPollInterval)
	sub.waitForCalls(t, 5)

	// The 5th observation is c-2's FIRST poll. If the counter had
	// persisted across the recreate (the bug we're guarding against),
	// this would already be RUNNING (counter would have been 4+1=5).
	// Post-fix it's PENDING (counter reset to 0; needs 5 more polls
	// under c-2 to flip).
	got := sub.snapshot()[4].GetState()
	if got != pb.ReplicaState_REPLICA_STATE_PENDING {
		t.Errorf("first poll under new container: state = %v, want PENDING (counter must reset on container change)", got)
	}
}
