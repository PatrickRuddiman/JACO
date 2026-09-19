package raftnode_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/raft/rafttest"
)

// noopFSM does nothing; sufficient for verifying that Apply returns indices.
type noopFSM struct{}

func (noopFSM) Apply(*hraft.Log) interface{}         { return nil }
func (noopFSM) Snapshot() (hraft.FSMSnapshot, error) { return noopSnapshot{}, nil }
func (noopFSM) Restore(io.ReadCloser) error          { return nil }

type noopSnapshot struct{}

func (noopSnapshot) Persist(sink hraft.SnapshotSink) error { return sink.Close() }
func (noopSnapshot) Release()                              {}

func TestNew_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  raftnode.Config
		want string
	}{
		{"no FSM", raftnode.Config{DataDir: "/tmp/x", BindAddr: "127.0.0.1:0", LocalID: "a"}, "FSM is required"},
		{"no LocalID", raftnode.Config{DataDir: "/tmp/x", BindAddr: "127.0.0.1:0", FSM: noopFSM{}}, "LocalID is required"},
		{"no DataDir", raftnode.Config{BindAddr: "127.0.0.1:0", LocalID: "a", FSM: noopFSM{}}, "DataDir is required"},
		{"no BindAddr", raftnode.Config{DataDir: "/tmp/x", LocalID: "a", FSM: noopFSM{}}, "BindAddr is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := raftnode.New(c.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !contains(err.Error(), c.want) {
				t.Fatalf("err = %v; want substring %q", err, c.want)
			}
		})
	}
}

func TestNewRequiresNodeCredentials(t *testing.T) {
	for _, name := range []string{"missing", "malformed", "wrong node identity", "wrong cluster CA"} {
		t.Run(name, func(t *testing.T) {
			dir, id := t.TempDir(), "node-a"
			if name != "missing" {
				cert, key := rafttest.NewCA(t)
				rafttest.Issue(t, dir, id, cert, key)
			}
			switch name {
			case "malformed":
				if err := os.WriteFile(filepath.Join(dir, "node", id+".crt"), []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong node identity":
				for _, ext := range []string{".crt", ".key"} {
					if err := os.Rename(filepath.Join(dir, "node", id+ext), filepath.Join(dir, "node", "node-b"+ext)); err != nil {
						t.Fatal(err)
					}
				}
				id = "node-b"
			case "wrong cluster CA":
				otherCA, _ := rafttest.NewCA(t)
				if err := os.WriteFile(filepath.Join(dir, "node", "ca.crt"), otherCA, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			n, err := raftnode.New(raftnode.Config{
				DataDir: dir, BindAddr: "127.0.0.1:0", LocalID: id,
				FSM: noopFSM{}, LogOutput: io.Discard,
			})
			if n != nil {
				t.Cleanup(func() { _ = n.Shutdown() })
			}
			if err == nil || !strings.Contains(err.Error(), "raft TLS") {
				t.Fatalf("New with %s credentials = %v; want raft TLS error", name, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "raft", "log.db")); !os.IsNotExist(err) {
				t.Fatalf("invalid credentials must not create raft state: %v", err)
			}
		})
	}
}

func TestBootstrapSingleNodeAndApply(t *testing.T) {
	dir := t.TempDir()
	cert, key := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", cert, key)
	n, err := raftnode.New(raftnode.Config{
		DataDir:   dir,
		BindAddr:  "127.0.0.1:0",
		LocalID:   "node-a",
		Bootstrap: true,
		FSM:       noopFSM{},
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })

	if !waitForLeader(t, n, 5*time.Second) {
		t.Fatalf("never became leader; state=%v", n.Raft.State())
	}

	idx, err := n.Apply([]byte("noop"), time.Second)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if idx == 0 {
		t.Errorf("Apply index should be > 0, got %d", idx)
	}

	if got := n.Leader(); got == "" {
		t.Errorf("Leader() empty after becoming leader")
	}
	if got, want := n.LocalAddr(), n.Leader(); got != want {
		t.Errorf("LocalAddr=%q, Leader=%q; want them equal on single-node bootstrap", got, want)
	}
}

func TestApplyDefaultTimeout(t *testing.T) {
	dir := t.TempDir()
	cert, key := rafttest.NewCA(t)
	rafttest.Issue(t, dir, "node-a", cert, key)
	n, err := raftnode.New(raftnode.Config{
		DataDir:   dir,
		BindAddr:  "127.0.0.1:0",
		LocalID:   "node-a",
		Bootstrap: true,
		FSM:       noopFSM{},
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	if !waitForLeader(t, n, 5*time.Second) {
		t.Fatalf("never became leader")
	}

	// timeout==0 should fall through to the package default; should still succeed.
	if _, err := n.Apply([]byte("x"), 0); err != nil {
		t.Fatalf("Apply(0): %v", err)
	}
}

func waitForLeader(t *testing.T, n *raftnode.Node, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.IsLeader() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
