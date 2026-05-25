// Command jacod is the long-running JACO daemon. Started by systemd; sits
// passive on its unix socket + (eventually) TLS-over-TCP listener until
// `jaco cluster init` or `jaco node join` populates raft state on disk.
//
// `jacod --version` prints the release version baked in by build/release.sh
// via -ldflags '-X main.version=…'.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/PatrickRuddiman/jaco/internal/daemon/config"
	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	"github.com/PatrickRuddiman/jaco/internal/daemon/netdetect"
	"github.com/PatrickRuddiman/jaco/internal/runtime/dockerx"
)

var version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to jacod.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := run(ctx, *configPath, os.Stderr); err != nil {
		log.Fatalf("jacod: %v", err)
	}
}

// run is the testable body of jacod. Returns when ctx is cancelled (via
// SIGTERM/SIGINT in production) or the gRPC server dies.
func run(ctx context.Context, configPath string, logOut io.Writer) error {
	logger := log.New(logOut, "jacod: ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", configPath, err)
	}
	logger.Printf("starting (version=%s data_dir=%s unix_socket=%s)",
		version, cfg.DataDir, cfg.UnixSocket)

	// Resolve advertise + bind addresses. When listen_addr / cluster_addr is
	// unspecified (0.0.0.0 / ::), jacod auto-detects a private-LAN-first face
	// and uses it for BOTH the advertise string (so peers can dial back) AND
	// the actual bind — the gRPC + raft control/data plane must not listen on
	// a world-reachable public face by default (issue #44). When the operator
	// pinned an explicit address, it's honored verbatim for bind + advertise.
	plan, err := resolveAdvertise(cfg.ListenAddr, cfg.ClusterAddr, logger)
	if err != nil {
		return err
	}
	listenBind := plan.listenBind
	clusterBind := plan.clusterBind
	listenAdvertise := plan.listenAdvertise
	clusterAdvertise := plan.clusterAdvertise

	// Best-effort docker connection. If the engine is unreachable, jacod
	// keeps the control plane running but skips the runtime reconciler —
	// useful for staging boxes without docker and for unit tests.
	var docker dockerx.Docker
	if d, dockerErr := dockerx.New(""); dockerErr != nil {
		logger.Printf("docker unreachable, runtime disabled: %v", dockerErr)
	} else {
		docker = d
	}

	server, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath:       cfg.UnixSocket,
		DataDir:              cfg.DataDir,
		ListenAddr:           listenBind,
		ListenAdvertiseAddr:  listenAdvertise,
		ClusterAddr:          clusterBind,
		ClusterAdvertiseAddr: clusterAdvertise,
		Docker:               docker,
		IPAMPool:             cfg.IPAMPool,
		ACMEEmail:            cfg.ACMEEmail,
		ACMECA:               cfg.ACMECAOrDefault(),
		ACMEEnabled:          cfg.ACMEEnabledOrDefault(),
		ACMESkipStaging:      cfg.ACMESkipStaging,
	})
	if err != nil {
		return fmt.Errorf("gRPC server: %w", err)
	}
	// Bug 005: if raft state already exists on disk, re-open it now so
	// the daemon resumes its existing membership instead of sitting at
	// "uninitialized" until an operator re-runs cluster init/join.
	// Hostname resolution matches the Cluster.Init handler's path.
	if _, statErr := os.Stat(cfg.DataDir + "/raft/log.db"); statErr == nil {
		hostname, hErr := os.Hostname()
		if hErr != nil {
			logger.Printf("hostname for raft resume: %v (staying uninitialized)", hErr)
		} else if err := server.OpenRaft(hostname, clusterBind, clusterAdvertise); err != nil {
			logger.Printf("auto-resume OpenRaft: %v (staying uninitialized)", err)
		} else {
			server.Gate().MarkInitialized()
			logger.Printf("resumed existing raft state for %s on %s (advertise %s)", hostname, clusterBind, clusterAdvertise)
		}
	} else {
		logger.Printf("listening on %s (uninitialized — run `jaco cluster init` or `jaco node join`)",
			server.SocketPath())
	}

	// Notify systemd we're up so Type=notify units complete activation.
	// No-op when not run under systemd (sd_notify returns notSent=false
	// with no err); logged for visibility.
	if sent, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		logger.Printf("sd_notify(READY=1): %v", err)
	} else if sent {
		logger.Printf("sd_notify(READY=1) sent")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve() }()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("gRPC server died: %w", err)
		}
	case <-ctx.Done():
		logger.Printf("signal received, shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Stop(shutdownCtx)
	logger.Printf("shutdown complete")
	return nil
}

// signalContext returns a context that cancels on SIGTERM / SIGINT.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		cancel()
		// Second signal exits hard.
		<-ch
		os.Exit(130)
	}()
	return ctx, cancel
}

// advertisePlan carries the effective bind + advertise host:port pairs for
// the gRPC (listen) and raft (cluster) endpoints, after resolving any
// unspecified (0.0.0.0 / ::) bind against the auto-detected private face.
//
// For each endpoint:
//   - When the operator pinned an explicit address, bind == advertise ==
//     the configured value (honored verbatim).
//   - When the configured bind is unspecified, both bind and advertise are
//     rebuilt as <detected-private-ip>:<configured-port>. The bind follows
//     the advertise face so the control/data plane is NOT exposed on a
//     public NIC by default (issue #44).
type advertisePlan struct {
	listenBind       string
	listenAdvertise  string // empty when listen bind was explicit (server falls back to bind)
	clusterBind      string
	clusterAdvertise string // empty when cluster bind was explicit
}

// resolveAdvertise computes the advertisePlan for the gRPC (listen) and
// raft (cluster) endpoints. When either is bound to an unspecified address
// (0.0.0.0 or ::), it auto-detects a private-LAN-first host IP via netdetect
// and synthesizes <ip>:<port> from the configured port — using it for BOTH
// the bind and the advertise string. When an endpoint is pinned to an
// explicit address it's honored verbatim (bind == advertise == configured),
// the documented escape hatch for overlay-only or multi-NIC topologies.
//
// netdetect never returns a public IP (issue #44): a host whose only
// routable face is public yields ErrNoCandidate here, surfacing the
// guidance below rather than silently exposing the cluster planes.
//
// Errors carry guidance pointing at /etc/jaco/jacod.yaml so the operator
// knows where to set an explicit value.
func resolveAdvertise(listenBind, clusterBind string, logger *log.Logger) (advertisePlan, error) {
	plan := advertisePlan{listenBind: listenBind, clusterBind: clusterBind}

	listenUnspec, listenPort, err := splitUnspecified(listenBind, "listen_addr")
	if err != nil {
		return advertisePlan{}, err
	}
	clusterUnspec, clusterPort, err := splitUnspecified(clusterBind, "cluster_addr")
	if err != nil {
		return advertisePlan{}, err
	}
	if !listenUnspec && !clusterUnspec {
		// Operator pinned both — bind + advertise are the configured values.
		return plan, nil
	}
	ip, iface, derr := netdetect.PickAdvertiseIP()
	if derr != nil {
		return advertisePlan{}, fmt.Errorf("auto-detect advertise IP: %w; set listen_addr/cluster_addr in /etc/jaco/jacod.yaml to a routable host:port", derr)
	}
	logger.Printf("advertise+bind=%s (auto-detected private face from %s) — control/data plane bound to this face, not 0.0.0.0", ip, iface)
	if listenUnspec {
		bind := net.JoinHostPort(ip.String(), listenPort)
		plan.listenBind = bind
		plan.listenAdvertise = bind
	}
	if clusterUnspec {
		bind := net.JoinHostPort(ip.String(), clusterPort)
		plan.clusterBind = bind
		plan.clusterAdvertise = bind
	}
	return plan, nil
}

// splitUnspecified returns whether addr's host part is an unspecified IP
// (0.0.0.0 / ::), plus the parsed port. Empty addr is treated as "not
// unspecified" — the caller's downstream code will produce an empty
// advertise and the gRPC server may even skip creating a listener.
func splitUnspecified(addr, fieldName string) (bool, string, error) {
	if addr == "" {
		return false, "", nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, "", fmt.Errorf("%s %q: %w", fieldName, addr, err)
	}
	parsed := net.ParseIP(host)
	if parsed == nil {
		// Hostname (not an IP literal) — treat as explicit; we don't
		// auto-detect for hostnames.
		return false, port, nil
	}
	return parsed.IsUnspecified(), port, nil
}

// defaultConfigPath returns the JACO_CONFIG env override or the documented
// default at /etc/jaco/jacod.yaml.
func defaultConfigPath() string {
	if v := os.Getenv("JACO_CONFIG"); v != "" {
		return v
	}
	return "/etc/jaco/jacod.yaml"
}
