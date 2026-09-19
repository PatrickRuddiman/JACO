package grpc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dgrpc "github.com/PatrickRuddiman/jaco/internal/daemon/grpc"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestMissingStateKeys(t *testing.T) {
	t.Setenv("JACO_STATE_KEY_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	socket := filepath.Join(t.TempDir(), "jacod.sock")
	server, err := dgrpc.New(dgrpc.Options{UnixSocketPath: socket, DataDir: t.TempDir()})
	if err == nil {
		server.Stop(context.Background())
		t.Fatal("daemon accepted missing external state keys")
	}
	if !strings.Contains(err.Error(), "state encryption") {
		t.Fatalf("expected a state encryption provisioning error, got %v", err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("failed key provisioning opened local-control socket: %v", err)
	}
}

func TestLegacyCacheBeforeSocket(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "ingress", "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("certificate.key"))
	if err := os.WriteFile(filepath.Join(cacheDir, hex.EncodeToString(sum[:])), []byte("synthetic-private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "j.sock")
	server, err := dgrpc.New(dgrpc.Options{UnixSocketPath: socket, DataDir: dir, Keys: testutil.StateKeys(t)})
	if err == nil {
		server.Stop(context.Background())
		t.Fatal("daemon accepted plaintext cache")
	}
	if !strings.Contains(err.Error(), "cache") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket must not be opened before cache validation: %v", err)
	}
}
