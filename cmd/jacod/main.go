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

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("jacod: load config %s: %v", *configPath, err)
	}
	log.Printf("jacod: starting (version=%s data_dir=%s unix_socket=%s)", version, cfg.DataDir, cfg.UnixSocket)

	server, err := dgrpc.New(dgrpc.Options{
		UnixSocketPath: cfg.UnixSocket,
	})
	if err != nil {
		log.Fatalf("jacod: gRPC server: %v", err)
	}
	log.Printf("jacod: listening on %s (uninitialized — run `jaco cluster init` or `jaco node join`)", server.SocketPath())

	rootCtx, cancel := signalContext()
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve() }()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("jacod: gRPC server died: %v", err)
		}
	case <-rootCtx.Done():
		log.Printf("jacod: signal received, shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Stop(shutdownCtx)
	log.Printf("jacod: shutdown complete")
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
