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

	"github.com/PatrickRuddiman/jaco/internal/daemon/config"
	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
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

	server, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: cfg.UnixSocket,
		DataDir:        cfg.DataDir,
		ClusterAddr:    cfg.ClusterAddr,
	})
	if err != nil {
		return fmt.Errorf("gRPC server: %w", err)
	}
	logger.Printf("listening on %s (uninitialized — run `jaco cluster init` or `jaco node join`)",
		server.SocketPath())

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
