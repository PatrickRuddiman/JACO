// Package health implements per-replica health observation. The Watcher
// polls each replica's container via dockerx.ContainerInspect, translates
// docker's State.Health.Status (or, for healthcheck-less services, the raw
// State.Status) into the closed pb.ReplicaState set, and submits a
// ReplicaObserved update on every poll so the ingress slice's
// `last_health_at < 10s` upstream-eligibility check stays fresh.
//
// Watcher is daemon-owned: one Watcher per host shared by every replica
// the runtime hosts; Start / Stop manage the per-replica goroutine.
package health

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// Poll cadences from the runtime slice §4. Fast cadence while the replica is
// still ramping (pending / pulling / degraded / failed); slow cadence once
// the replica reports running.
const (
	FastPollInterval = 1 * time.Second
	SlowPollInterval = 5 * time.Second

	// HealthyConsecutiveCount is how many `running` polls (no healthcheck
	// declared) JACO requires before flipping to ReplicaState_RUNNING.
	HealthyConsecutiveCount = 5
)

// Clock abstracts time.Now / time.After so tests don't burn wall time.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// SystemClock returns the production Clock implementation.
func SystemClock() Clock { return systemClock{} }

// SubmitFn writes a ReplicaObserved update through the control plane. The
// daemon wires this to either a local raft.Apply (when self is the leader)
// or an Internal.Submit gRPC call (when self is a follower).
type SubmitFn func(ctx context.Context, obs *pb.ReplicaObserved) error

// Watcher manages a per-replica health-poll goroutine for the local host.
type Watcher struct {
	docker dockerx.Docker
	submit SubmitFn
	clock  Clock

	mu       sync.Mutex
	watchers map[string]*replicaWatcher
}

type replicaWatcher struct {
	replicaID      string
	containerID    string
	hasHealthcheck bool
	cancel         context.CancelFunc

	// loop-owned state
	lastState         pb.ReplicaState
	consecutiveRunning int
	startedAt         time.Time
}

// NewWatcher constructs a Watcher. clock may be nil to use SystemClock.
func NewWatcher(d dockerx.Docker, submit SubmitFn, clock Clock) *Watcher {
	if clock == nil {
		clock = SystemClock()
	}
	return &Watcher{
		docker:   d,
		submit:   submit,
		clock:    clock,
		watchers: map[string]*replicaWatcher{},
	}
}

// Start begins polling the container identified by containerID. If a watcher
// for replicaID already exists, it is cancelled and replaced — useful when a
// rolling update gives the replica a new container id.
func (w *Watcher) Start(parent context.Context, replicaID, containerID string, hasHealthcheck bool) {
	w.Stop(replicaID)

	ctx, cancel := context.WithCancel(parent)
	rw := &replicaWatcher{
		replicaID:      replicaID,
		containerID:    containerID,
		hasHealthcheck: hasHealthcheck,
		cancel:         cancel,
		lastState:      pb.ReplicaState_REPLICA_STATE_PENDING,
	}
	w.mu.Lock()
	w.watchers[replicaID] = rw
	w.mu.Unlock()

	go w.loop(ctx, rw)
}

// Stop cancels the per-replica goroutine. Idempotent.
func (w *Watcher) Stop(replicaID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rw, ok := w.watchers[replicaID]; ok {
		rw.cancel()
		delete(w.watchers, replicaID)
	}
}

// StopAll cancels every active per-replica watcher (used by the daemon on
// graceful shutdown).
func (w *Watcher) StopAll() {
	w.mu.Lock()
	for id, rw := range w.watchers {
		rw.cancel()
		delete(w.watchers, id)
	}
	w.mu.Unlock()
}

// Active reports the number of currently-running replica watchers.
func (w *Watcher) Active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.watchers)
}

func (w *Watcher) loop(ctx context.Context, rw *replicaWatcher) {
	for {
		interval := FastPollInterval
		if rw.lastState == pb.ReplicaState_REPLICA_STATE_RUNNING {
			interval = SlowPollInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(interval):
		}

		obs := w.poll(ctx, rw)
		if obs == nil {
			continue
		}
		_ = w.submit(ctx, obs)
	}
}

// poll inspects the container and builds the next ReplicaObserved. Returns
// nil only when ctx is cancelled mid-inspect.
func (w *Watcher) poll(ctx context.Context, rw *replicaWatcher) *pb.ReplicaObserved {
	info, err := w.docker.ContainerInspect(ctx, rw.containerID)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		// Treat inspect failures as `failed/inspect_failed` so the ingress
		// drops the replica from rotation; the scheduler can decide whether
		// to restart.
		rw.lastState = pb.ReplicaState_REPLICA_STATE_FAILED
		return &pb.ReplicaObserved{
			Id:           rw.replicaID,
			State:        pb.ReplicaState_REPLICA_STATE_FAILED,
			Code:         "inspect_failed",
			Message:      err.Error(),
			ContainerId:  rw.containerID,
			LastHealthAt: timestamppb.New(w.clock.Now()),
		}
	}

	state, code, exitCode := classify(info, rw)
	rw.lastState = state

	obs := &pb.ReplicaObserved{
		Id:           rw.replicaID,
		State:        state,
		Code:         code,
		ContainerId:  rw.containerID,
		LastHealthAt: timestamppb.New(w.clock.Now()),
	}
	if !rw.startedAt.IsZero() {
		obs.StartedAt = timestamppb.New(rw.startedAt)
	} else if state == pb.ReplicaState_REPLICA_STATE_RUNNING {
		rw.startedAt = w.clock.Now()
		obs.StartedAt = timestamppb.New(rw.startedAt)
	}
	if exitCode != 0 {
		obs.Details = map[string]string{"exit_code": strconv.Itoa(exitCode)}
	}
	return obs
}

// classify maps docker's container state into JACO's closed ReplicaState set.
// rw's consecutive-running counter is mutated when no healthcheck is declared
// and the container is running; once HealthyConsecutiveCount is reached the
// replica flips to RUNNING.
func classify(info types.ContainerJSON, rw *replicaWatcher) (pb.ReplicaState, string, int) {
	if info.State == nil {
		return pb.ReplicaState_REPLICA_STATE_FAILED, "no_state", 0
	}

	// Terminal: an exited container is always failed.
	if info.State.Status == "exited" {
		return pb.ReplicaState_REPLICA_STATE_FAILED, "container_exited", info.State.ExitCode
	}

	if rw.hasHealthcheck && info.State.Health != nil {
		switch info.State.Health.Status {
		case "starting":
			return pb.ReplicaState_REPLICA_STATE_PENDING, "", 0
		case "healthy":
			return pb.ReplicaState_REPLICA_STATE_RUNNING, "", 0
		case "unhealthy":
			return pb.ReplicaState_REPLICA_STATE_DEGRADED, "", 0
		}
	}

	// No healthcheck declared: gate RUNNING on N consecutive running polls.
	if info.State.Status == "running" {
		rw.consecutiveRunning++
		if rw.consecutiveRunning >= HealthyConsecutiveCount {
			return pb.ReplicaState_REPLICA_STATE_RUNNING, "", 0
		}
		return pb.ReplicaState_REPLICA_STATE_PENDING, "", 0
	}
	rw.consecutiveRunning = 0
	return pb.ReplicaState_REPLICA_STATE_PENDING, "", 0
}
