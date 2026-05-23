// Package grpc builds the gRPC server jacod listens on. v1 opens a unix
// socket listener; TLS-over-TCP for cross-host control lands once the
// daemon transitions through Init/Join and has a real cluster CA cert
// (later iters of task 38).
package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cpadmission "github.com/PatrickRuddiman/jaco/internal/controlplane/admission"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/daemon/admission"
	dnsmgr "github.com/PatrickRuddiman/jaco/internal/discovery/dns"
	"github.com/PatrickRuddiman/jaco/internal/discovery/firewall"
	"github.com/PatrickRuddiman/jaco/internal/discovery/ipam"
	"github.com/PatrickRuddiman/jaco/internal/discovery/wgmesh"
	"github.com/PatrickRuddiman/jaco/internal/ingress/rebuild"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
	"github.com/PatrickRuddiman/jaco/internal/runtime/health"
	"github.com/PatrickRuddiman/jaco/internal/runtime/reconciler"
	"github.com/PatrickRuddiman/jaco/internal/scheduler"
	schedhealth "github.com/PatrickRuddiman/jaco/internal/scheduler/health"
	"github.com/PatrickRuddiman/jaco/internal/scheduler/rollout"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
	hraft "github.com/hashicorp/raft"
	"google.golang.org/grpc/credentials"
)

// Server bundles the daemon-side gRPC server with its unix socket listener
// + the InitGate that governs which RPCs accept while uninitialized.
type Server struct {
	gs           *grpc.Server
	listener     net.Listener // unix socket — local control
	tcpListener  net.Listener // cross-host control; nil when ListenAddr unset
	gate         *admission.InitGate
	socketPath   string
	tcpAddr      string // resolved listener bind address (the port may be 0 → ephemeral)
	tcpAdvertise string // host:port to gossip in publishSelf + NodeJoin (falls back to tcpAddr)

	cluster *clusterServer
	tokens  *tokensProxy
	deploy  *deployProxy
	audit   *auditProxy
	watch   *watchProxy

	// Populated by OpenRaft after Cluster.Init or Cluster.Join lands. The
	// pre-OpenRaft state is "raft handle nil; RPCs that need raft return
	// Unavailable + cluster_uninitialized via the gate".
	raftMu  sync.RWMutex
	raft    *raftnode.Node
	state   *state.State
	brokers *watch.Registry
	fsm     *fsm.FSM

	// ipamAllocator is the single per-leader /24 allocator. Shared between
	// the Internal.EnsureSubnet RPC handler and the local reconciler's
	// ensureSubnet closure so their read-nextFree-apply sequences serialize
	// on one mutex (separate instances would race on the free pool). Set in
	// startSubsystems under raftMu.
	ipamAllocator *ipam.IPAM

	// subsystemsCancel cancels every steady-state goroutine spawned by
	// OpenRaft (scheduler.Run, restarter.Run, etc). Reset to nil after Stop
	// drains subsystemsWG.
	subsystemsCancel context.CancelFunc
	subsystemsWG     sync.WaitGroup

	// logger receives subsystem errors so they surface in jacod's stderr
	// instead of disappearing into goroutine panics. nil → log.Default().
	logger *log.Logger

	// docker is the optional runtime engine handle. nil → no runtime
	// reconciler is spawned in startSubsystems.
	docker dockerx.Docker

	// ipamPool is the /16 the leader allocates per-host /24s from when it
	// handles Internal.EnsureSubnet.
	ipamPool string

	// tlsDyn holds the live server cert for the cross-host TCP listener.
	// Nil when no TCP listener was opened. RebindTLS swaps the cert in
	// after OpenRaft persists the cluster-CA-signed node cert.
	tlsDyn *dynamicTLS

	mu      sync.Mutex
	started bool
}

// Options configures Server.
type Options struct {
	// UnixSocketPath is the path to the local-control socket. Parent dir
	// is created if missing; existing socket file is removed.
	UnixSocketPath string

	// SocketMode is the permission mask applied to the socket file
	// (default 0o660 — owner+group rw).
	SocketMode os.FileMode

	// DataDir is the daemon's $JACO_DATA_DIR. Cluster.Init writes raft
	// state under $DataDir/raft and certs under $DataDir/node.
	DataDir string

	// ListenAddr is the cross-host control-plane listener (TCP). Peers
	// dial this for Cluster.{Status,Join} during cluster formation and
	// for ongoing operator RPCs. Empty → no cross-host listener (single
	// node only).
	//
	// v0 ships plaintext TCP — Tailscale / WireGuard is expected to wrap
	// the connection. TLS-with-cluster-CA is a follow-up iter.
	ListenAddr string

	// ListenAdvertiseAddr is the host:port peers should be told to dial
	// for ListenAddr. When empty, defaults to ListenAddr. Must be a
	// routable IP (not 0.0.0.0); cmd/jacod resolves this from
	// netdetect.PickAdvertiseIP when the operator's config has an
	// unspecified bind.
	ListenAdvertiseAddr string

	// ClusterAddr is the raft TCP transport listen address. Used by
	// Cluster.Init to bind raft so peers can dial.
	ClusterAddr string

	// ClusterAdvertiseAddr is the host:port raft tells peers to dial for
	// its transport. Same auto-detect story as ListenAdvertiseAddr;
	// empty → ClusterAddr.
	ClusterAdvertiseAddr string

	// Hostname overrides os.Hostname() at handler time. Tests use this;
	// production leaves it empty.
	Hostname string

	// Logger receives subsystem errors. nil → log.Default(). Tests pass a
	// log.Logger writing to an io.Discard to suppress noise.
	Logger *log.Logger

	// Docker is the runtime engine handle. nil → skip runtime wiring (the
	// daemon still runs the control plane + scheduler, but doesn't create
	// containers). cmd/jacod wires dockerx.New; tests usually pass nil or
	// an in-memory fake.
	Docker dockerx.Docker

	// IPAMPool is the /16 the leader allocates per-host /24s from when it
	// handles Internal.EnsureSubnet. Empty → ipam.DefaultPoolCIDR.
	IPAMPool string
}

// ipamPoolOrDefault returns the configured IPAM pool, or the JACO default
// when the operator left it empty.
func ipamPoolOrDefault(pool string) string {
	if pool == "" {
		return ipam.DefaultPoolCIDR
	}
	return pool
}

// New builds a Server. Doesn't start anything yet — call Serve.
func New(opts Options) (*Server, error) {
	if opts.UnixSocketPath == "" {
		return nil, errors.New("UnixSocketPath is required")
	}
	if opts.SocketMode == 0 {
		opts.SocketMode = 0o660
	}

	if err := os.MkdirAll(filepath.Dir(opts.UnixSocketPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket parent: %w", err)
	}
	// Remove any stale socket file from a previous run.
	_ = os.Remove(opts.UnixSocketPath)

	lis, err := net.Listen("unix", opts.UnixSocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", opts.UnixSocketPath, err)
	}
	if err := os.Chmod(opts.UnixSocketPath, opts.SocketMode); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	gate := admission.New()
	// stateAccessor returns the current state.State once OpenRaft has
	// populated it. Captured by lazyUnary / lazyStream below so the
	// admission interceptor is constructed (and reads the live token
	// store) on every post-init request.
	server := &Server{} // forward-declare so the closures capture it
	stateAccessor := func() *state.State {
		server.raftMu.RLock()
		defer server.raftMu.RUnlock()
		return server.state
	}
	lazyUnary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// UnauthMethods (Status, NodeJoin) bypass the bearer check
		// regardless of whether state is populated — Status doesn't read
		// state.Tokens and NodeJoin authenticates via the body's
		// join_token. This keeps Cluster.Status callable even in tests
		// that flip the gate manually without driving OpenRaft.
		if cpadmission.UnauthMethods[info.FullMethod] {
			return handler(ctx, req)
		}
		st := stateAccessor()
		if st == nil {
			// Defensive: post-init handler ran before state hookup. Should
			// not happen because OpenRaft populates state before flipping
			// the gate, but fail closed rather than skipping admission.
			return nil, errStateUnavailable
		}
		return cpadmission.UnaryInterceptor(st)(ctx, req, info, handler)
	}
	lazyStream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if cpadmission.UnauthMethods[info.FullMethod] {
			return handler(srv, ss)
		}
		st := stateAccessor()
		if st == nil {
			return errStateUnavailable
		}
		return cpadmission.StreamInterceptor(st)(srv, ss, info, handler)
	}
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(gate.UnaryInterceptor(lazyUnary)),
		grpc.StreamInterceptor(gate.StreamInterceptor(lazyStream)),
	)

	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	// Optional cross-host TCP listener. Empty ListenAddr → single-node
	// daemon (unix socket only). We open this NOW so a failure surfaces
	// before Init/Join rather than mid-flight. The TCP listener is
	// always TLS-wrapped — pre-Init with a self-signed bootstrap cert
	// (joiners dial with InsecureSkipVerify; the join_token is the
	// trust anchor); rebindTLS swaps in the cluster-CA-signed cert
	// after OpenRaft persists certs.
	var tcpLis net.Listener
	var tcpAddr string
	var dynTLS *dynamicTLS
	if opts.ListenAddr != "" {
		raw, err := net.Listen("tcp", opts.ListenAddr)
		if err != nil {
			_ = lis.Close()
			return nil, fmt.Errorf("listen tcp %s: %w", opts.ListenAddr, err)
		}
		btls, dyn, err := bootstrapTLSConfig(opts.Hostname)
		if err != nil {
			_ = lis.Close()
			_ = raw.Close()
			return nil, fmt.Errorf("bootstrap TLS: %w", err)
		}
		tcpLis = tls.NewListener(raw, btls)
		tcpAddr = raw.Addr().String()
		dynTLS = dyn
	}

	tcpAdvertise := opts.ListenAdvertiseAddr
	if tcpAdvertise == "" {
		tcpAdvertise = tcpAddr
	}
	*server = Server{
		gs:           gs,
		listener:     lis,
		tcpListener:  tcpLis,
		gate:         gate,
		socketPath:   opts.UnixSocketPath,
		tcpAddr:      tcpAddr,
		tcpAdvertise: tcpAdvertise,
		logger:       logger,
		docker:       opts.Docker,
		tlsDyn:       dynTLS,
		ipamPool:     ipamPoolOrDefault(opts.IPAMPool),
	}
	cluster := &clusterServer{
		gate:          gate,
		dataDir:       opts.DataDir,
		bindAddr:      opts.ClusterAddr,
		advertiseAddr: opts.ClusterAdvertiseAddr,
		hostname:      opts.Hostname,
		server:        server,
	}
	server.cluster = cluster
	pb.RegisterClusterServer(gs, cluster)
	pb.RegisterInternalServer(gs, &internalServer{server: server})

	// Register the four lazily-resolved control-plane services. Their
	// target handlers get filled by wireControlPlane in startSubsystems
	// after OpenRaft populates state + raft.
	server.tokens = &tokensProxy{}
	server.deploy = &deployProxy{server: server}
	server.audit = &auditProxy{}
	server.watch = &watchProxy{}
	pb.RegisterTokensServer(gs, server.tokens)
	pb.RegisterDeployServer(gs, server.deploy)
	pb.RegisterAuditServer(gs, server.audit)
	pb.RegisterWatchServer(gs, server.watch)

	return server, nil
}

// OpenRaft opens the persisted raft state and populates the Server's
// raft/state/brokers/fsm handles. Called from Cluster.Init (after
// bootstrap.Run) and from Cluster.Join (after persistJoin). Idempotent —
// returns nil + leaves existing handles alone if already opened.
//
// advertiseAddr is the host:port peers should dial; empty falls back to
// bindAddr (legacy single-IP path for tests). When bindAddr is 0.0.0.0:N
// the caller MUST supply a real advertiseAddr or raft will refuse to
// start with "local bind address is not advertisable".
func (s *Server) OpenRaft(hostname, bindAddr, advertiseAddr string) error {
	s.raftMu.Lock()
	defer s.raftMu.Unlock()
	if s.raft != nil {
		return nil
	}
	if hostname == "" {
		return fmt.Errorf("OpenRaft: hostname is required")
	}
	if bindAddr == "" {
		return fmt.Errorf("OpenRaft: bindAddr is required")
	}
	if s.cluster == nil || s.cluster.dataDir == "" {
		return fmt.Errorf("OpenRaft: dataDir is required")
	}

	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)

	node, err := raftnode.New(raftnode.Config{
		DataDir:       s.cluster.dataDir,
		BindAddr:      bindAddr,
		AdvertiseAddr: advertiseAddr,
		LocalID:       hostname,
		Bootstrap:     false, // raft state already on disk
		FSM:           f,
	})
	if err != nil {
		return fmt.Errorf("raftnode.New: %w", err)
	}

	s.raft = node
	s.state = st
	s.brokers = brokers
	s.fsm = f

	// Swap the bootstrap cert for the cluster-CA-signed node cert on the
	// cross-host TCP listener. Best-effort — if the node cert isn't on
	// disk (e.g. test paths that skip Init's persistJoin), the listener
	// keeps the bootstrap cert and operators must keep using
	// InsecureSkipVerify.
	if s.tlsDyn != nil && s.cluster != nil && s.cluster.dataDir != "" {
		if cert, err := clusterNodeCert(s.cluster.dataDir, hostname); err == nil {
			s.tlsDyn.swap(cert)
		} else {
			s.logger.Printf("rebindTLS: %v (keeping bootstrap cert)", err)
		}
	}

	s.startSubsystems(node, st, brokers, hostname)
	return nil
}

// startSubsystems spins up every per-host goroutine that depends on raft +
// state being open. Called from OpenRaft under s.raftMu (so the goroutines
// see fully-populated handles). All goroutines self-cancel when
// s.subsystemsCancel fires from Stop.
//
// v0 wires scheduler.Run (desired-state reconciler, leader-only) and
// scheduler/health.Restarter.Run (restart policy, leader-only). Runtime,
// discovery, and ingress wiring lands in subsequent iters.
func (s *Server) startSubsystems(node *raftnode.Node, st *state.State, brokers *watch.Registry, hostname string) {
	ctx, cancel := context.WithCancel(context.Background())
	s.subsystemsCancel = cancel

	// Fill the control-plane proxies now that state + raft are populated.
	s.wireControlPlane()

	apply := func(cmd []byte) error {
		_, err := node.Apply(cmd, 0)
		return err
	}

	// Single shared /24 allocator (we already hold raftMu here). The
	// EnsureSubnet RPC handler and the reconciler's ensureSubnet closure
	// both use this instance so allocations serialize on one mutex.
	if allocator, err := ipam.New(st, apply, s.ipamPool); err != nil {
		s.logger.Printf("ipam allocator init: %v (per-host subnet allocation disabled)", err)
	} else {
		s.ipamAllocator = allocator
	}

	rollouts := rollout.New(st, apply, rollout.SystemClock())
	sched := scheduler.New(st, brokers, node, apply, rollouts)
	s.subsystemsWG.Add(1)
	go func() {
		defer s.subsystemsWG.Done()
		if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("scheduler.Run exited: %v", err)
		}
	}()

	restarter := schedhealth.New(st, brokers, node, apply)
	s.subsystemsWG.Add(1)
	go func() {
		defer s.subsystemsWG.Done()
		if err := restarter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("scheduler/health.Restarter.Run exited: %v", err)
		}
	}()

	// Discovery: WireGuard mesh sync. Skipped when the kernel WG module
	// isn't reachable via wgctrl (typical on unprivileged dev hosts and
	// containers without CAP_NET_ADMIN).
	if err := wgmesh.IsKernelAvailable(); err == nil {
		privKey, pubKey, err := wgmesh.LoadOrGenerateKeypair(s.cluster.dataDir)
		if err != nil {
			s.logger.Printf("wgmesh keypair: %v (mesh sync skipped)", err)
		} else {
			// Bring up the wg interface if it doesn't exist yet. Skips
			// gracefully without CAP_NET_ADMIN — Syncer's tick logs the
			// resulting ConfigureDevice failure once.
			if err := wgmesh.EnsureInterface(wgmesh.DefaultInterface); err != nil {
				s.logger.Printf("wgmesh.EnsureInterface: %v (mesh sync best-effort)", err)
			}
			// Publish our wireguard pubkey + gRPC address through raft so
			// peers see them after a restart / initial Init. Bug 011:
			// followers must retry until the leader's grpc_address has
			// propagated; first try usually fails on follower because
			// state.Nodes doesn't yet have the leader's grpc addr.
			s.subsystemsWG.Add(1)
			go func() {
				defer s.subsystemsWG.Done()
				s.publishSelfRetry(ctx, node, st, hostname, pubKey)
			}()

			syncer := &wgmesh.Syncer{
				State:        st,
				SelfHostname: hostname,
				PrivateKey:   privKey,
				Logger:       s.logger,
			}
			s.subsystemsWG.Add(1)
			go func() {
				defer s.subsystemsWG.Done()
				if err := syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					s.logger.Printf("wgmesh.Syncer.Run exited: %v", err)
				}
			}()
		}
	} else {
		s.logger.Printf("wgmesh kernel unavailable (%v), mesh sync skipped", err)
	}

	// Discovery: nftables firewall reconciler. Skipped when `nft` isn't
	// available on PATH or the kernel netfilter API is unreachable.
	if err := firewall.IsAvailable(); err == nil {
		fw := &firewall.Reconciler{
			Lister:  firewall.DefaultLister(),
			Applier: firewall.DefaultApplier(),
			Audit: func(ctx context.Context, code string, details map[string]string) error {
				cmd := &pb.Command{
					Identity: "firewall",
					Payload: &pb.Command_AuditAppend{AuditAppend: &pb.AuditAppend{
						Event: &pb.AuditEvent{
							Type:    auditTypeFromString(code),
							Payload: details,
						},
					}},
				}
				data, err := proto.Marshal(cmd)
				if err != nil {
					return err
				}
				_, err = node.Apply(data, 0)
				return err
			},
			UpdateStatus: func(ctx context.Context, statusStr, reason string) error {
				cmd := &pb.Command{
					Identity: "firewall",
					Payload: &pb.Command_NodeStatusUpdate{NodeStatusUpdate: &pb.NodeStatusUpdate{
						Hostname: hostname,
						Status:   nodeStatusFromString(statusStr),
						Details:  map[string]string{"reason": reason},
					}},
				}
				data, err := proto.Marshal(cmd)
				if err != nil {
					return err
				}
				_, err = node.Apply(data, 0)
				return err
			},
			Render: func() firewall.RuleInput {
				var subs []firewall.Subnet
				for _, sn := range st.Subnets.List() {
					subs = append(subs, firewall.Subnet{
						Deployment: sn.GetDeployment(),
						Network:    sn.GetNetwork(),
						CIDR:       sn.GetCidr(),
					})
				}
				return firewall.RuleInput{
					Subnets:      subs,
					WGPort:       wgmesh.DefaultListenPort,
					GrpcPort:     7000,
					IngressPorts: []int{80, 443},
				}
			},
		}
		s.subsystemsWG.Add(1)
		go func() {
			defer s.subsystemsWG.Done()
			if err := fw.Loop(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("firewall.Reconciler.Loop exited: %v", err)
			}
		}()
	} else {
		s.logger.Printf("firewall unavailable (%v), drift detector skipped", err)
	}

	// Discovery: per-bridge DNS Manager. Spawns a UDP+TCP listener per
	// (deployment, network) subnet on the bridge gateway IP. Skips
	// gracefully when listeners can't bind (no docker bridge yet, or
	// missing CAP_NET_BIND_SERVICE).
	dnsMgr := &dnsmgr.Manager{State: st, Brokers: brokers, Logger: s.logger, Hostname: hostname}
	s.subsystemsWG.Add(1)
	go func() {
		defer s.subsystemsWG.Done()
		if err := dnsMgr.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Printf("dns.Manager.Run exited: %v", err)
		}
	}()

	// Ingress: the Reloader fires whenever Routes / ReplicasObserved /
	// Certs / ChallengeTokens change, rebuilds the Caddy JSON config,
	// and execs `caddy reload`. Skipped when the caddy binary isn't on
	// PATH (typical on dev hosts) — the Builder still runs but the
	// Loader is a no-op so the daemon doesn't crash.
	if caddyAvailable() {
		// Construct + register the raft-backed Storage with caddy so
		// configs referencing module "jaco" resolve at load time
		// (task 33 deferral). hostname is the lessee in cert locks.
		jacoStorage := storage.New(st, apply, hostname, nil)
		storage.SetDefaultStorage(jacoStorage)

		// ACME email empty until we plumb config; operators set it via
		// the future jacod.yaml.acme_email field.
		rl := rebuild.New(brokers, ingressBuilder(st, ""), ingressLoader())
		s.subsystemsWG.Add(1)
		go func() {
			defer s.subsystemsWG.Done()
			if err := rl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("ingress.Reloader.Run exited: %v", err)
			}
		}()
	} else {
		s.logger.Printf("caddy binary not found on PATH, ingress reload loop skipped")
	}

	// Runtime reconciler: skipped when no Docker handle was injected (the
	// daemon still serves the control plane + scheduler in that mode). On
	// hosts where docker is unreachable, opts.Docker should already be
	// nil — cmd/jacod logs a warning + continues.
	if s.docker != nil {
		// SubmitFn writes ReplicaObserved back through raft.Apply directly
		// (leader path). Follower-side Internal.Submit forwarding lands in
		// a later iter — for now the runtime works on whichever node is
		// also the leader.
		// Bug 011 diagnostic: log every SubmitFn error so silent failures
		// in follower → leader forwarding surface in journal. health.Watcher
		// doesn't log SubmitFn errors itself.
		var submitErrLogOnce sync.Once
		submit := func(ctx context.Context, obs *pb.ReplicaObserved) error {
			cmd := &pb.Command{
				Identity: "runtime",
				Payload:  &pb.Command_ReplicaObservedUpdate{ReplicaObservedUpdate: &pb.ReplicaObservedUpdate{Replica: obs}},
			}
			data, err := proto.Marshal(cmd)
			if err != nil {
				return fmt.Errorf("marshal: %w", err)
			}
			if _, applyErr := node.Apply(data, 0); applyErr == nil {
				return nil
			} else if !errors.Is(applyErr, hraft.ErrNotLeader) {
				submitErrLogOnce.Do(func() {
					s.logger.Printf("submit raft.Apply (non-leader path): %v", applyErr)
				})
				return applyErr
			}
			// Not the leader — forward to whichever node is. Look up
			// the leader's gRPC address from state.Nodes and dial
			// Internal.Submit there. Plaintext; same v0 wire model.
			leaderAddr := leaderGRPCAddr(st, node)
			if leaderAddr == "" {
				submitErrLogOnce.Do(func() {
					s.logger.Printf("submit: no leader gRPC address known (state.Nodes lookup empty)")
				})
				return fmt.Errorf("submit: no leader gRPC address known")
			}
			conn, dialErr := grpc.NewClient(leaderAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
			if dialErr != nil {
				submitErrLogOnce.Do(func() {
					s.logger.Printf("submit dial leader %s: %v", leaderAddr, dialErr)
				})
				return fmt.Errorf("submit: dial leader %s: %w", leaderAddr, dialErr)
			}
			defer conn.Close()
			_, rpcErr := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data})
			if rpcErr != nil {
				submitErrLogOnce.Do(func() {
					s.logger.Printf("submit Internal.Submit to %s: %v", leaderAddr, rpcErr)
				})
			}
			return rpcErr
		}
		// ensureSubnet allocates this host's /24 for a replica's network.
		// On the leader it uses the shared allocator directly; on a follower
		// it forwards to the leader's Internal.EnsureSubnet. Pool exhaustion
		// is normalized to reconciler.ErrSubnetPoolExhausted; everything else
		// is transient and retried on the reconciler's next tick.
		ensureSubnet := func(ctx context.Context, deployment, network, host string) (string, error) {
			if node.IsLeader() {
				allocator := s.IPAMAllocator()
				if allocator == nil {
					return "", fmt.Errorf("ensureSubnet: ipam allocator not initialized")
				}
				sn, err := allocator.Allocate(deployment, network, host)
				if err != nil {
					if ipam.IsExhausted(err) {
						return "", reconciler.ErrSubnetPoolExhausted
					}
					return "", err
				}
				return sn.GetCidr(), nil
			}
			leaderAddr := leaderGRPCAddr(st, node)
			if leaderAddr == "" {
				return "", fmt.Errorf("ensureSubnet: no leader gRPC address known")
			}
			conn, dialErr := grpc.NewClient(leaderAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
			if dialErr != nil {
				return "", fmt.Errorf("ensureSubnet: dial leader %s: %w", leaderAddr, dialErr)
			}
			defer conn.Close()
			resp, rpcErr := pb.NewInternalClient(conn).EnsureSubnet(ctx, &pb.EnsureSubnetRequest{
				Deployment: deployment, Network: network, Host: host,
			})
			if rpcErr != nil {
				if status.Code(rpcErr) == codes.ResourceExhausted {
					return "", reconciler.ErrSubnetPoolExhausted
				}
				return "", rpcErr
			}
			return resp.GetCidr(), nil
		}
		rec := reconciler.New(s.docker, st, brokers, hostname, health.SubmitFn(submit), reconciler.EnsureSubnetFn(ensureSubnet), s.logger)
		s.subsystemsWG.Add(1)
		go func() {
			defer s.subsystemsWG.Done()
			if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("runtime.Reconciler.Run exited: %v", err)
			}
		}()
	}
}

// Raft returns the daemon's raft handle. nil pre-OpenRaft.
func (s *Server) Raft() *raftnode.Node {
	s.raftMu.RLock()
	defer s.raftMu.RUnlock()
	return s.raft
}

// State returns the daemon's state.State. nil pre-OpenRaft.
func (s *Server) State() *state.State {
	s.raftMu.RLock()
	defer s.raftMu.RUnlock()
	return s.state
}

// IPAMAllocator returns the shared per-leader /24 allocator. nil pre-OpenRaft.
func (s *Server) IPAMAllocator() *ipam.IPAM {
	s.raftMu.RLock()
	defer s.raftMu.RUnlock()
	return s.ipamAllocator
}

// Serve blocks until Stop is called or one of the listeners errors. When a
// cross-host TCP listener is configured, it runs alongside the unix socket
// on the same grpc.Server (so Cluster RPCs are visible identically on both
// transports).
func (s *Server) Serve() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("server already started")
	}
	s.started = true
	s.mu.Unlock()

	errs := make(chan error, 2)
	go func() {
		err := s.gs.Serve(s.listener)
		if errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
		errs <- err
	}()
	if s.tcpListener != nil {
		go func() {
			err := s.gs.Serve(s.tcpListener)
			if errors.Is(err, grpc.ErrServerStopped) {
				err = nil
			}
			errs <- err
		}()
	}
	// Return the first non-nil error (or nil if both shut down cleanly).
	first := <-errs
	if s.tcpListener != nil {
		// Drain the other one so its goroutine doesn't leak.
		select {
		case <-errs:
		default:
		}
	}
	return first
}

// Stop performs a graceful shutdown.
func (s *Server) Stop(ctx context.Context) {
	stopped := make(chan struct{})
	go func() {
		s.gs.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		s.gs.Stop()
	}
	_ = os.Remove(s.socketPath)

	// Cancel steady-state goroutines BEFORE shutting raft so they don't
	// race against a nil raft handle. WaitGroup drains with a 5s budget;
	// after that we proceed regardless to avoid hanging the daemon.
	s.raftMu.Lock()
	cancel := s.subsystemsCancel
	s.subsystemsCancel = nil
	s.raftMu.Unlock()
	if cancel != nil {
		cancel()
		done := make(chan struct{})
		go func() { s.subsystemsWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.logger.Printf("subsystems shutdown timed out after 5s")
		}
	}

	// Close raft + release the bolt-store file lock so a follow-on jacod
	// boot (or test) can re-open the same data dir.
	s.raftMu.Lock()
	if s.raft != nil {
		_ = s.raft.Shutdown()
		s.raft = nil
	}
	s.raftMu.Unlock()
}

// Gate returns the InitGate so callers can flip MarkInitialized after a
// successful Init / Join.
func (s *Server) Gate() *admission.InitGate { return s.gate }

// SocketPath returns the path the daemon is listening on.
func (s *Server) SocketPath() string { return s.socketPath }

// TCPAddr returns the resolved cross-host listener bind address (after
// net.Listen substituted any :0 port). Empty when ListenAddr was unset.
func (s *Server) TCPAddr() string { return s.tcpAddr }

// TCPAdvertiseAddr returns the host:port to gossip to peers. Equals
// opts.ListenAdvertiseAddr when explicitly set; otherwise falls back to
// the bound TCPAddr. Used in NodeJoin requests and the publishSelf path.
func (s *Server) TCPAdvertiseAddr() string {
	if s.tcpAdvertise != "" {
		return s.tcpAdvertise
	}
	return s.tcpAddr
}

// publishSelf raft-Applies a NodeUpdateSelf so peers see our current
// wireguard pubkey + grpc address. Direct apply on the leader; follower
// forwarding via Internal.Submit at the leader's gRPC address. Returns
// nil on success, the underlying error otherwise (caller retries).
func (s *Server) publishSelf(ctx context.Context, node *raftnode.Node, st *state.State, hostname string, pubKey wgtypesKey) error {
	cmd := &pb.Command{
		Identity: "node-update-self",
		Payload: &pb.Command_NodeUpdateSelf{NodeUpdateSelf: &pb.NodeUpdateSelf{
			Hostname:        hostname,
			WireguardPubkey: pubKey[:],
			GrpcAddress:     s.TCPAdvertiseAddr(),
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := node.Apply(data, 0); err == nil {
		return nil
	} else if !errors.Is(err, hraft.ErrNotLeader) {
		return fmt.Errorf("apply: %w", err)
	}
	leaderAddr := leaderGRPCAddr(st, node)
	if leaderAddr == "" {
		return fmt.Errorf("no leader gRPC address known")
	}
	conn, err := grpc.NewClient(leaderAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		return fmt.Errorf("dial leader: %w", err)
	}
	defer conn.Close()
	if _, err := pb.NewInternalClient(conn).Submit(ctx, &pb.SubmitRequest{CommandBytes: data}); err != nil {
		return fmt.Errorf("forward: %w", err)
	}
	return nil
}

// publishSelfRetry calls publishSelf with exponential backoff until it
// succeeds or ctx is cancelled. Bug 011: first attempt usually fails on
// followers because state.Nodes hasn't seen the leader's
// grpc_address yet; retry until it has.
func (s *Server) publishSelfRetry(ctx context.Context, node *raftnode.Node, st *state.State, hostname string, pubKey wgtypesKey) {
	backoff := 200 * time.Millisecond
	const maxBackoff = 5 * time.Second
	for {
		err := s.publishSelf(ctx, node, st, hostname, pubKey)
		if err == nil {
			s.logger.Printf("publishSelf succeeded for %s", hostname)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// wgtypesKey is the fixed-size byte array wgctrl's wgtypes.Key uses.
// Pulled into a local alias so server.go doesn't need to import wgtypes
// just for the publishSelf signature.
type wgtypesKey = [32]byte

// leaderGRPCAddr returns the leader's cross-host gRPC address by matching
// raft.Leader() (a raft transport addr) against state.Nodes[].Address and
// returning the matched Node.GrpcAddress. Empty when the leader isn't
// known or its gRPC address hasn't been recorded yet.
func leaderGRPCAddr(st *state.State, node *raftnode.Node) string {
	if st == nil || node == nil {
		return ""
	}
	raftLeader := string(node.Leader())
	if raftLeader == "" {
		return ""
	}
	for _, n := range st.Nodes.List() {
		if n.GetAddress() == raftLeader {
			return n.GetGrpcAddress()
		}
	}
	return ""
}

// auditTypeFromString maps the firewall reconciler's event-code strings
// onto the pb.AuditEventType enum. Unknown codes fall through to the
// generic ISOLATION_RULESET_RECONCILED bucket.
func auditTypeFromString(code string) pb.AuditEventType {
	switch code {
	case "ISOLATION_RULESET_RECONCILED":
		return pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED
	case "ISOLATION_UNAVAILABLE":
		return pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_UNAVAILABLE
	}
	return pb.AuditEventType_AUDIT_EVENT_TYPE_ISOLATION_RULESET_RECONCILED
}

// nodeStatusFromString maps the firewall reconciler's status strings onto
// the pb.NodeStatus enum.
func nodeStatusFromString(s string) pb.NodeStatus {
	switch s {
	case "ready":
		return pb.NodeStatus_NODE_STATUS_READY
	case "isolation_unavailable":
		return pb.NodeStatus_NODE_STATUS_ISOLATION_UNAVAILABLE
	}
	return pb.NodeStatus_NODE_STATUS_READY
}

// errStateUnavailable is returned by the lazy admission interceptor when it
// fires before OpenRaft has populated state. Should be unreachable in
// practice because the gate doesn't dispatch post-init handlers until
// MarkInitialized fires (which OpenRaft does after assigning state).
var errStateUnavailable = status.Error(codes.Unavailable, "state_unavailable: daemon raft state not populated yet")
