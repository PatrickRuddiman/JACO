// Package reconciler is the per-host runtime loop. It subscribes to the
// ReplicasDesired watch broker, filters events down to host=self, and
// drives lifecycle.Start / Stop / Remove + the health.Watcher per replica
// id.
//
// Unlike the scheduler (which runs on the leader and writes ReplicaDesired
// records), the reconciler runs on EVERY node and only operates on its own
// host's slice of those records. It never writes to raft directly; it does
// emit ReplicaObserved updates through the provided SubmitFn.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/logging"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	"github.com/PatrickRuddiman/jaco/internal/runtime/pull"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// resolveDNSServers returns one gateway IP per declared network, looked
// up via state.Subnets + bridge.GatewayIP. Networks the daemon doesn't
// yet know a CIDR for are silently skipped. host is the local node — the
// per-host subnet whose gateway the local container resolves against.
func resolveDNSServers(st *state.State, deployment, host string, networks []string) []string {
	var out []string
	for _, netname := range networks {
		// Network names in spec are docker-network names (jaco_<dep>_<net>);
		// state.Subnets keys by (deployment, network, host) — pull just the
		// network suffix from the docker name.
		net := bridge.NetworkNameFromDockerName(netname)
		if net == "" {
			continue
		}
		sn, ok := st.Subnets.Get(state.SubnetKey(deployment, net, host))
		if !ok {
			continue
		}
		gw, err := bridge.GatewayIP(sn.GetCidr())
		if err == nil {
			out = append(out, gw)
		}
	}
	return out
}

// ErrSubnetPoolExhausted is the sentinel an EnsureSubnetFn returns when the
// IPAM pool can't satisfy a per-host /24. The reconciler maps it to a FAILED
// ReplicaObserved rather than retrying forever. Any other error is treated as
// transient (e.g. no leader yet) and retried on the next tick.
var ErrSubnetPoolExhausted = errors.New("subnet pool exhausted")

// EnsureSubnetFn allocates (idempotently) the per-host /24 for
// (deployment, network, host) and returns its CIDR. The daemon wires this to
// the leader's ipam allocator directly, or to Internal.EnsureSubnet on a
// follower; either way it maps pool exhaustion to ErrSubnetPoolExhausted.
type EnsureSubnetFn func(ctx context.Context, deployment, network, host string) (cidr string, err error)

// startHandle tracks one in-flight async replica start so the reconcile loop
// can dedup re-dispatches and cancel a start (on remove / host-migration /
// shutdown).
type startHandle struct {
	raftIndex uint64
	cancel    context.CancelFunc
}

// Reconciler is the per-host runtime driver.
type Reconciler struct {
	docker       dockerx.Docker
	state        *state.State
	brokers      *watch.Registry
	hostname     string
	watcher      *health.Watcher
	submit       health.SubmitFn
	ensureSubnet EnsureSubnetFn
	logger       *slog.Logger

	// starting tracks in-flight async starts by replica id. A replica start
	// does slow, blocking work — an image pull that retries forever on an
	// unreachable registry, plus container create/start — so it runs in a
	// per-replica goroutine. Doing it inline in Run's select loop let a single
	// stuck pull freeze the loop and stall every other replica + the 30s safety
	// tick on the node; dispatching it async keeps the loop responsive.
	startMu  sync.Mutex
	starting map[string]*startHandle

	// netMu serializes the per-network ensureSubnet+bridge.Ensure step across
	// concurrent start goroutines so two replicas sharing a network don't race
	// to create the same docker bridge.
	netMu sync.Mutex
}

// New constructs a Reconciler. submit is the function the per-replica
// health.Watcher uses to publish ReplicaObserved updates back through the
// control plane (raft.Apply on the leader, Internal.Submit on followers).
// ensureSubnet allocates the per-host /24 for a replica's networks before its
// bridges are created.
func New(docker dockerx.Docker, st *state.State, brokers *watch.Registry, hostname string, submit health.SubmitFn, ensureSubnet EnsureSubnetFn, logger *slog.Logger) *Reconciler {
	base := logger
	logger = logging.Subsystem(base, "runtime/reconciler").With(logging.KeyNode, hostname)
	watcher := health.NewWatcher(docker, submit, nil, nil)
	watcher.Logger = logging.Subsystem(base, "runtime/health").With(logging.KeyNode, hostname)
	return &Reconciler{
		docker:       docker,
		state:        st,
		brokers:      brokers,
		hostname:     hostname,
		watcher:      watcher,
		submit:       submit,
		ensureSubnet: ensureSubnet,
		logger:       logger,
		starting:     map[string]*startHandle{},
	}
}

// Run blocks until ctx is cancelled. Performs an initial orphan sweep +
// resync against state.ReplicasDesired.List(host=self), then subscribes to
// the ReplicasDesired broker for the steady-state loop.
func (r *Reconciler) Run(ctx context.Context) error {
	sub := r.brokers.ReplicasDesired.Subscribe()
	defer sub.Cancel()

	// Orphan sweep: stop+remove any container labeled with our cluster_id
	// but whose replica_id isn't in our desired set. Then start every
	// desired replica for this host (lifecycle.Start is idempotent).
	//
	// On a fresh Init the FSM is still replaying when startSubsystems
	// fires, so cluster meta + replicas may not be populated yet — log
	// at debug-only and let the steady-state loop pick up the replicas
	// via the ReplicasDesired watch.
	if err := r.orphanSweep(ctx); err != nil {
		// The only "error" is "cluster meta not populated" on a fresh boot,
		// which is expected and self-heals via the watch — log at debug so
		// it's available when diagnosing but doesn't clutter steady-state.
		r.logger.Debug("initial orphan sweep skipped", "error", err)
	}
	r.resync(ctx)

	// Bug 008: 30s safety tick that re-walks state.ReplicasDesired
	// host=self so the runtime recovers from out-of-band container
	// removal + missed watch events. Bug 016: also run orphanSweep so
	// containers whose desired record now points to another host (e.g.
	// after a drain migration) get stop+removed even if the watch event
	// was missed.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.cancelAllStarts()
			r.watcher.StopAll()
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			r.handle(ctx, ev)
		case <-ticker.C:
			if err := r.orphanSweep(ctx); err != nil {
				r.logger.Debug("safety-tick orphan sweep skipped", "error", err)
			}
			r.resync(ctx)
		}
	}
}

// orphanSweep stops+removes any local container labeled with our cluster_id
// whose replica_id isn't present in state.ReplicasDesired filtered to
// host=self. Run at boot and on every safety tick so the local docker
// state converges on the desired set even when watch events are missed.
func (r *Reconciler) orphanSweep(ctx context.Context) error {
	meta := r.state.Cluster.Get()
	if meta == nil || meta.GetClusterId() == "" {
		// Pre-Init shouldn't happen because the daemon only spawns this
		// goroutine after OpenRaft, but be defensive.
		return errors.New("cluster meta not populated; skipping orphan sweep")
	}
	expected := map[string]bool{}
	for _, rep := range r.state.ReplicasDesired.List() {
		if rep.GetHost() == r.hostname {
			expected[rep.GetId()] = true
		}
	}
	removed, err := lifecycle.Reconcile(ctx, r.docker, meta.GetClusterId(), expected)
	if err != nil {
		return err
	}
	for _, rid := range removed {
		r.logger.Info("orphan-swept container", logging.KeyReplicaID, rid)
		r.watcher.Stop(rid)
	}
	return nil
}

// OrphanSweep is the test-visible alias for orphanSweep.
func (r *Reconciler) OrphanSweep(ctx context.Context) error { return r.orphanSweep(ctx) }

// resync dispatches an async start for every desired replica host=self. Used
// on boot and after a watch broker overflow (KindResync). Dispatch is
// non-blocking + deduped, so a replica whose start is already in flight (e.g.
// stuck on an image pull) is skipped rather than piling up goroutines.
func (r *Reconciler) resync(ctx context.Context) {
	for _, rep := range r.state.ReplicasDesired.List() {
		if rep.GetHost() != r.hostname {
			continue
		}
		r.dispatchStart(ctx, rep)
	}
}

func (r *Reconciler) handle(ctx context.Context, ev watch.Event[*pb.ReplicaDesired]) {
	switch ev.Kind {
	case watch.KindAdded, watch.KindUpdated:
		rep := ev.After
		if rep.GetHost() != r.hostname {
			// If the replica moved off this host (Updated with new host),
			// stop + remove the container we still have running locally.
			if ev.Kind == watch.KindUpdated && ev.Before != nil && ev.Before.GetHost() == r.hostname {
				r.stopReplica(ctx, ev.Before)
			}
			return
		}
		r.dispatchStart(ctx, rep)
	case watch.KindRemoved:
		rep := ev.Before
		if rep == nil || rep.GetHost() != r.hostname {
			return
		}
		r.stopReplica(ctx, rep)
	case watch.KindResync:
		r.resync(ctx)
	}
}

// runStart projects the ReplicaDesired into a compose.ContainerSpec and brings
// the container up, then begins health-watching it. It runs inside a
// per-replica goroutine (see dispatchStart): workCtx scopes the slow,
// cancelable work (subnet alloc, image pull, create/start) so a stuck pull can
// be cancelled on remove/shutdown, while watchCtx is the long-lived context the
// health watcher runs under (it must outlive the start operation).
func (r *Reconciler) runStart(workCtx, watchCtx context.Context, rep *pb.ReplicaDesired) error {
	dep, ok := r.state.Deployments.Get(rep.GetDeployment())
	if !ok {
		return fmt.Errorf("deployment %s missing from state", rep.GetDeployment())
	}
	meta := r.state.Cluster.Get()
	clusterID := ""
	if meta != nil {
		clusterID = meta.GetClusterId()
	}
	project, err := compose.LoadBytes(dep.GetComposeYaml(), "runtime-compose.yml")
	if err != nil {
		return fmt.Errorf("compose parse: %w", err)
	}
	// name is the single source of truth: the service name equals the compose key.
	composeService := rep.GetService()
	// Confirm the service is declared in the deployment.
	found := false
	for _, svc := range dep.GetServices() {
		if svc.GetName() == composeService {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("service %s missing from deployment %s", rep.GetService(), rep.GetDeployment())
	}
	svcCfg, ok := project.Services[composeService]
	if !ok {
		return fmt.Errorf("compose service %q not in project", composeService)
	}
	// Image override: scheduler may have pinned an image differing from
	// compose (e.g. rolled to a new tag). Honor it.
	if rep.GetImage() != "" {
		svcCfg.Image = rep.GetImage()
	}
	spec := compose.ToContainerSpec(svcCfg, compose.SpecOptions{
		ClusterID:    clusterID,
		Deployment:   rep.GetDeployment(),
		Service:      rep.GetService(),
		ReplicaID:    rep.GetId(),
		ReplicaIndex: int(rep.GetIndex()),
		RaftIndex:    rep.GetRaftIndex(),
	})
	// Bug 007: ensure each declared docker network exists on the local
	// engine before lifecycle.Start tries to NetworkConnect.
	// Idempotent; safe to call on every reconcile.
	for _, netname := range spec.Networks {
		netSuffix := bridge.NetworkNameFromDockerName(netname)
		if netSuffix == "" {
			continue
		}
		// Allocate this host's /24 for the network before creating the
		// bridge. The leader computes it; a follower forwards to the leader.
		cidr, err := r.ensureSubnet(workCtx, rep.GetDeployment(), netSuffix, r.hostname)
		if err != nil {
			if errors.Is(err, ErrSubnetPoolExhausted) {
				r.failReplica(workCtx, rep, netSuffix, "subnet_pool_exhausted")
				return nil
			}
			// Transient (e.g. no leader yet) — leave the replica unstarted;
			// the watch / 30s safety tick retries.
			return fmt.Errorf("ensureSubnet %s/%s on %s: %w", rep.GetDeployment(), netSuffix, r.hostname, err)
		}
		// Serialize bridge creation: concurrent start goroutines sharing a
		// network must not race to NetworkCreate the same docker bridge.
		r.netMu.Lock()
		_, ensErr := bridge.Ensure(workCtx, r.docker, rep.GetDeployment(), netSuffix, cidr, clusterID, r.logger)
		r.netMu.Unlock()
		if ensErr != nil {
			r.logger.Error("bridge.Ensure failed",
				logging.KeyReplicaID, rep.GetId(), logging.KeyDeployment, rep.GetDeployment(), "network", netSuffix, "error", ensErr)
		}
	}

	// Per-bridge DNS resolvers (task 27 deferral). For each declared
	// network, look up the subnet CIDR in state.Subnets and compute the
	// gateway IP; that's where the daemon's discovery/dns Manager binds.
	spec.DNSServers = resolveDNSServers(r.state, rep.GetDeployment(), r.hostname, spec.Networks)

	// Surface image-pull state. Without this, a pull that can't succeed (e.g.
	// an unreachable registry) retries forever inside Start with zero
	// visibility — the replica simply never appears in `jaco status` and the
	// node logs nothing (issue #66). Report PULLING on the first attempt and
	// PENDING + the error on each failure so the stuck replica is visible.
	onPull := func(s pull.State, attempt int, _ time.Time, lastErr error) {
		switch s {
		case pull.StatePulling:
			if attempt == 1 {
				r.observePull(workCtx, rep, pb.ReplicaState_REPLICA_STATE_PULLING, "pulling", "")
			}
		case pull.StateFailed:
			r.logger.Warn("image pull failed; will retry",
				logging.KeyReplicaID, rep.GetId(), logging.KeyDeployment, rep.GetDeployment(),
				"image", spec.Image, "attempt", attempt, "error", lastErr)
			r.observePull(workCtx, rep, pb.ReplicaState_REPLICA_STATE_PENDING, "image_pull_failed",
				fmt.Sprintf("%s: %v", spec.Image, lastErr))
		}
	}
	containerID, err := lifecycle.StartWithPullState(workCtx, r.docker, spec, onPull, lifecycle.IsolationGate{
		State:        r.state,
		SelfHostname: r.hostname,
	})
	if err != nil {
		return fmt.Errorf("lifecycle.Start: %w", err)
	}
	r.logger.Info("replica container started",
		logging.KeyReplicaID, rep.GetId(), logging.KeyDeployment, rep.GetDeployment(),
		"service", rep.GetService(), "container_id", containerID)
	r.watcher.Start(watchCtx, rep.GetId(), containerID, spec.Healthcheck != nil)
	return nil
}

// dispatchStart launches (or refreshes) the async start for rep. Non-blocking:
// the pull+create+start runs in a goroutine so a slow/stuck pull never freezes
// the reconcile loop. Deduped by replica id — a re-dispatch for the same
// desired raft_index while a start is already in flight is a no-op; a changed
// raft_index (e.g. an image roll) cancels and supersedes the in-flight start.
func (r *Reconciler) dispatchStart(parent context.Context, rep *pb.ReplicaDesired) {
	id := rep.GetId()
	r.startMu.Lock()
	if h, ok := r.starting[id]; ok {
		if h.raftIndex == rep.GetRaftIndex() {
			r.startMu.Unlock()
			return // already starting this exact desired spec
		}
		h.cancel() // desired changed — supersede the in-flight start
	}
	workCtx, cancel := context.WithCancel(parent)
	h := &startHandle{raftIndex: rep.GetRaftIndex(), cancel: cancel}
	r.starting[id] = h
	r.startMu.Unlock()

	go func() {
		defer cancel()
		defer func() {
			r.startMu.Lock()
			if cur, ok := r.starting[id]; ok && cur == h {
				delete(r.starting, id)
			}
			r.startMu.Unlock()
		}()
		if err := r.runStart(workCtx, parent, rep); err != nil && !errors.Is(err, context.Canceled) {
			r.logger.Error("start replica failed",
				logging.KeyReplicaID, id, logging.KeyDeployment, rep.GetDeployment(), "error", err)
		}
	}()
}

// cancelStart cancels and forgets any in-flight start for id.
func (r *Reconciler) cancelStart(id string) {
	r.startMu.Lock()
	if h, ok := r.starting[id]; ok {
		h.cancel()
		delete(r.starting, id)
	}
	r.startMu.Unlock()
}

// cancelAllStarts cancels every in-flight start (Run shutdown).
func (r *Reconciler) cancelAllStarts() {
	r.startMu.Lock()
	for id, h := range r.starting {
		h.cancel()
		delete(r.starting, id)
	}
	r.startMu.Unlock()
}

// failReplica publishes a FAILED ReplicaObserved with the given code so the
// operator sees the offending replica (and the scheduler stops retrying it
// forever). Used when subnet allocation can't be satisfied.
func (r *Reconciler) failReplica(ctx context.Context, rep *pb.ReplicaDesired, network, code string) {
	r.logger.Warn("marking replica FAILED",
		logging.KeyReplicaID, rep.GetId(), logging.KeyDeployment, rep.GetDeployment(),
		"network", network, "code", code)
	if r.submit == nil {
		return
	}
	if err := r.submit(ctx, &pb.ReplicaObserved{
		Id:    rep.GetId(),
		State: pb.ReplicaState_REPLICA_STATE_FAILED,
		Code:  code,
		Host:  r.hostname,
		Details: map[string]string{
			"deployment": rep.GetDeployment(),
			"network":    network,
			"host":       r.hostname,
		},
	}); err != nil {
		r.logger.Error("failReplica submit failed", logging.KeyReplicaID, rep.GetId(), "error", err)
	}
}

// observePull submits a ReplicaObserved capturing image-pull state so a pull
// that's in progress or stuck is visible in `jaco status` + audit instead of
// silently absent. No-op when submit is unset (tests that don't wire it).
func (r *Reconciler) observePull(ctx context.Context, rep *pb.ReplicaDesired, st pb.ReplicaState, code, reason string) {
	if r.submit == nil {
		return
	}
	details := map[string]string{"deployment": rep.GetDeployment(), "host": r.hostname}
	if reason != "" {
		details["reason"] = reason
	}
	if err := r.submit(ctx, &pb.ReplicaObserved{
		Id:      rep.GetId(),
		State:   st,
		Code:    code,
		Host:    r.hostname,
		Details: details,
	}); err != nil {
		r.logger.Error("observePull submit failed", logging.KeyReplicaID, rep.GetId(), "error", err)
	}
}

// stopReplica is the symmetric teardown — stops the health watcher then
// stop+removes the container.
func (r *Reconciler) stopReplica(ctx context.Context, rep *pb.ReplicaDesired) {
	// Cancel any in-flight async start first (e.g. a replica removed while its
	// image pull is still retrying) so the goroutine doesn't create a container
	// we're about to tear down.
	r.cancelStart(rep.GetId())
	r.watcher.Stop(rep.GetId())
	r.logger.Info("stopping replica container",
		logging.KeyReplicaID, rep.GetId(), logging.KeyDeployment, rep.GetDeployment())
	if err := lifecycle.Stop(ctx, r.docker, rep.GetId(), 10); err != nil {
		r.logger.Error("lifecycle.Stop failed", logging.KeyReplicaID, rep.GetId(), "error", err)
	}
	if err := lifecycle.Remove(ctx, r.docker, rep.GetId()); err != nil {
		r.logger.Error("lifecycle.Remove failed", logging.KeyReplicaID, rep.GetId(), "error", err)
	}
}

// Watcher exposes the per-replica health watcher (useful for tests).
func (r *Reconciler) Watcher() *health.Watcher { return r.watcher }
