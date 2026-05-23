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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/PatrickRuddiman/jaco/internal/daemon/config"
	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
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
		UnixSocketPath: cfg.UnixSocket,
		DataDir:        cfg.DataDir,
		ListenAddr:     cfg.ListenAddr,
		ClusterAddr:    cfg.ClusterAddr,
		Docker:         docker,
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
		} else if err := server.OpenRaft(hostname, cfg.ClusterAddr); err != nil {
			logger.Printf("auto-resume OpenRaft: %v (staying uninitialized)", err)
		} else {
			server.Gate().MarkInitialized()
			logger.Printf("resumed existing raft state for %s on %s", hostname, cfg.ClusterAddr)
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

// defaultConfigPath returns the JACO_CONFIG env override or the documented
// default at /etc/jaco/jacod.yaml.
func defaultConfigPath() string {
	if v := os.Getenv("JACO_CONFIG"); v != "" {
		return v
	}
	return "/etc/jaco/jacod.yaml"
}
