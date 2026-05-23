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
	"log"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/discovery/bridge"
	"github.com/PatrickRuddiman/jaco/internal/runtime/compose"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	"github.com/PatrickRuddiman/jaco/internal/runtime/lifecycle"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// resolveDNSServers returns one gateway IP per declared network, looked
// up via state.Subnets + bridge.GatewayIP. Networks the daemon doesn't
// yet know a CIDR for are silently skipped.
func resolveDNSServers(st *state.State, deployment string, networks []string) []string {
	var out []string
	for _, netname := range networks {
		// Network names in spec are docker-network names (jaco_<dep>_<net>);
		// state.Subnets keys by (deployment, network) — pull just the network
		// suffix from the docker name.
		net := bridge.NetworkNameFromDockerName(netname)
		if net == "" {
			continue
		}
		sn, ok := st.Subnets.Get(state.SubnetKey(deployment, net))
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

// Reconciler is the per-host runtime driver.
type Reconciler struct {
	docker   dockerx.Docker
	state    *state.State
	brokers  *watch.Registry
	hostname string
	watcher  *health.Watcher
	logger   *log.Logger
}

// New constructs a Reconciler. submit is the function the per-replica
// health.Watcher uses to publish ReplicaObserved updates back through the
// control plane (raft.Apply on the leader, Internal.Submit on followers).
func New(docker dockerx.Docker, st *state.State, brokers *watch.Registry, hostname string, submit health.SubmitFn, logger *log.Logger) *Reconciler {
	if logger == nil {
		logger = log.Default()
	}
	return &Reconciler{
		docker:   docker,
		state:    st,
		brokers:  brokers,
		hostname: hostname,
		watcher:  health.NewWatcher(docker, submit, nil),
		logger:   logger,
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
		// Silent: the only "error" is "cluster meta not populated" on
		// a fresh boot, which is expected and self-heals via the watch.
		_ = err
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
			r.watcher.StopAll()
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			r.handle(ctx, ev)
		case <-ticker.C:
			if err := r.orphanSweep(ctx); err != nil {
				_ = err
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
		r.logger.Printf("orphan-swept container replica_id=%s", rid)
		r.watcher.Stop(rid)
	}
	return nil
}

// OrphanSweep is the test-visible alias for orphanSweep.
func (r *Reconciler) OrphanSweep(ctx context.Context) error { return r.orphanSweep(ctx) }

// resync applies every desired replica host=self via lifecycle.Start. Used
// on boot and after a watch broker overflow (KindResync).
func (r *Reconciler) resync(ctx context.Context) {
	for _, rep := range r.state.ReplicasDesired.List() {
		if rep.GetHost() != r.hostname {
			continue
		}
		if err := r.startReplica(ctx, rep); err != nil {
			r.logger.Printf("start replica %s: %v", rep.GetId(), err)
		}
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
		if err := r.startReplica(ctx, rep); err != nil {
			r.logger.Printf("start replica %s: %v", rep.GetId(), err)
		}
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

// startReplica projects the ReplicaDesired into a compose.ContainerSpec
// and calls lifecycle.Start, then begins health-watching the container.
func (r *Reconciler) startReplica(ctx context.Context, rep *pb.ReplicaDesired) error {
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
		sn, ok := r.state.Subnets.Get(state.SubnetKey(rep.GetDeployment(), netSuffix))
		if !ok {
			continue
		}
		if _, err := bridge.Ensure(ctx, r.docker, rep.GetDeployment(), netSuffix, sn.GetCidr(), clusterID); err != nil {
			r.logger.Printf("bridge.Ensure %s/%s: %v", rep.GetDeployment(), netSuffix, err)
		}
	}

	// Per-bridge DNS resolvers (task 27 deferral). For each declared
	// network, look up the subnet CIDR in state.Subnets and compute the
	// gateway IP; that's where the daemon's discovery/dns Manager binds.
	spec.DNSServers = resolveDNSServers(r.state, rep.GetDeployment(), spec.Networks)
	containerID, err := lifecycle.Start(ctx, r.docker, spec, lifecycle.IsolationGate{
		State:        r.state,
		SelfHostname: r.hostname,
	})
	if err != nil {
		return fmt.Errorf("lifecycle.Start: %w", err)
	}
	r.watcher.Start(ctx, rep.GetId(), containerID, spec.Healthcheck != nil)
	return nil
}

// stopReplica is the symmetric teardown — stops the health watcher then
// stop+removes the container.
func (r *Reconciler) stopReplica(ctx context.Context, rep *pb.ReplicaDesired) {
	r.watcher.Stop(rep.GetId())
	if err := lifecycle.Stop(ctx, r.docker, rep.GetId(), 10); err != nil {
		r.logger.Printf("lifecycle.Stop %s: %v", rep.GetId(), err)
	}
	if err := lifecycle.Remove(ctx, r.docker, rep.GetId()); err != nil {
		r.logger.Printf("lifecycle.Remove %s: %v", rep.GetId(), err)
	}
}

// Watcher exposes the per-replica health watcher (useful for tests).
func (r *Reconciler) Watcher() *health.Watcher { return r.watcher }
