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
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/logging"
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

// SubmitFn writes a ReplicaObserved update through the control plane. The
// daemon wires this to either a local raft.Apply (when self is the leader)
// or an Internal.Submit gRPC call (when self is a follower).
type SubmitFn func(ctx context.Context, obs *pb.ReplicaObserved) error

// Watcher manages a per-replica health-poll goroutine for the local host.
type Watcher struct {
	docker dockerx.Docker
	submit SubmitFn
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time

	// Logger logs replica health-state transitions at INFO. nil → discard.
	// Set by the reconciler after construction.
	Logger *slog.Logger

	mu       sync.Mutex
	watchers map[string]*replicaWatcher
}

func (w *Watcher) log() *slog.Logger {
	if w.Logger == nil {
		return logging.Discard()
	}
	return w.Logger
}

// logTransition logs at INFO when newState differs from the watcher's last
// observed state (pending→running→failed etc). No-op on steady-state polls.
func (w *Watcher) logTransition(rw *replicaWatcher, newState pb.ReplicaState, code string) {
	if newState == rw.lastState {
		return
	}
	w.log().Info("replica health transition",
		logging.KeyReplicaID, rw.replicaID,
		"from", stateString(rw.lastState), "to", stateString(newState), "code", code)
}

// stateString renders a pb.ReplicaState as a short lowercase label for logs.
func stateString(s pb.ReplicaState) string {
	switch s {
	case pb.ReplicaState_REPLICA_STATE_PENDING:
		return "pending"
	case pb.ReplicaState_REPLICA_STATE_RUNNING:
		return "running"
	case pb.ReplicaState_REPLICA_STATE_DEGRADED:
		return "degraded"
	case pb.ReplicaState_REPLICA_STATE_FAILED:
		return "failed"
	}
	return "unspecified"
}

type replicaWatcher struct {
	replicaID      string
	containerID    string
	hasHealthcheck bool
	cancel         context.CancelFunc

	// loop-owned state
	lastState          pb.ReplicaState
	consecutiveRunning int
	startedAt          time.Time
}

// NewWatcher constructs a Watcher. now / after may both be nil to use the
// real time.Now / time.After; tests install fakes to avoid burning wall
// time on the per-replica poll cadence.
func NewWatcher(d dockerx.Docker, submit SubmitFn, now func() time.Time, after func(time.Duration) <-chan time.Time) *Watcher {
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	return &Watcher{
		docker:   d,
		submit:   submit,
		now:      now,
		after:    after,
		watchers: map[string]*replicaWatcher{},
	}
}

// Start begins polling the container identified by containerID. Idempotent
// for the same (replicaID, containerID) pair: when a watcher is already
// running for that pair, Start returns immediately and the existing
// goroutine continues. When containerID differs from the live watcher's
// (a recreate fired — e.g. the scheduler's spec_hash drift detector
// caught an env change, see #148) the old watcher is cancelled and a
// fresh one starts.
//
// The same-container idempotency is load-bearing for the fallback path
// in classify: a healthcheck-less container must accumulate
// HealthyConsecutiveCount consecutive "running" polls before flipping
// to RUNNING (line 285-289). Pre-#152 the reconciler called Start at
// the end of every runStart, which fires on every ReplicaDesired event,
// resync, and safety tick. In a stack with stuck depends_on waiters
// runStart re-dispatches faster than 5 polls, so unconditionally
// recreating the replicaWatcher (with consecutiveRunning back at 0)
// meant the counter could never reach HealthyConsecutiveCount and
// healthcheck-less replicas stayed PENDING indefinitely. hasHealthcheck
// is not part of the identity check because it is derived from the
// compose spec baked into the container at create time — a transition
// from "compose declares healthcheck" to "compose does not" is
// impossible without a recreate, which would have changed containerID.
func (w *Watcher) Start(parent context.Context, replicaID, containerID string, hasHealthcheck bool) {
	w.mu.Lock()
	if existing, ok := w.watchers[replicaID]; ok && existing.containerID == containerID {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

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
		case <-w.after(interval):
		}

		obs := w.poll(ctx, rw)
		if obs == nil {
			continue
		}
		if err := w.submit(ctx, obs); err != nil && ctx.Err() == nil {
			w.log().Warn("replica health submit failed",
				logging.KeyReplicaID, rw.replicaID,
				"state", stateString(obs.GetState()), "error", err)
		}
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
		w.logTransition(rw, pb.ReplicaState_REPLICA_STATE_FAILED, "inspect_failed")
		rw.lastState = pb.ReplicaState_REPLICA_STATE_FAILED
		return &pb.ReplicaObserved{
			Id:           rw.replicaID,
			State:        pb.ReplicaState_REPLICA_STATE_FAILED,
			Code:         "inspect_failed",
			Message:      err.Error(),
			ContainerId:  rw.containerID,
			LastHealthAt: timestamppb.New(w.now()),
		}
	}

	state, code, exitCode := classify(info, rw)
	w.logTransition(rw, state, code)
	rw.lastState = state

	obs := &pb.ReplicaObserved{
		Id:           rw.replicaID,
		State:        state,
		Code:         code,
		ContainerId:  rw.containerID,
		LastHealthAt: timestamppb.New(w.now()),
	}
	if !rw.startedAt.IsZero() {
		obs.StartedAt = timestamppb.New(rw.startedAt)
	} else if state == pb.ReplicaState_REPLICA_STATE_RUNNING {
		rw.startedAt = w.now()
		obs.StartedAt = timestamppb.New(rw.startedAt)
	}
	if exitCode != 0 {
		if obs.Details == nil {
			obs.Details = map[string]string{}
		}
		obs.Details["exit_code"] = strconv.Itoa(exitCode)
	}
	// Per-network container IPs (issue #28): the DNS responder reads
	// Details["ip.<dockerNetwork>"] to answer service names with the right
	// per-host IP. NetworkSettings.Networks is keyed by docker network name.
	if info.NetworkSettings != nil {
		for netName, ep := range info.NetworkSettings.Networks {
			if ep == nil || ep.IPAddress == "" {
				continue
			}
			if obs.Details == nil {
				obs.Details = map[string]string{}
			}
			obs.Details["ip."+netName] = ep.IPAddress
		}
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
