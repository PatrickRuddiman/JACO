package raftnode_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestStartupRefusesIncompleteHistory(t *testing.T) {
	for _, artifact := range []string{seal.IncompleteMarker, filepath.Join("raft", "snapshots", "retained", "state.bin")} {
		t.Run(filepath.Base(artifact), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, artifact)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("synthetic-retained-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			brokers := watch.NewRegistry()
			node, err := raftnode.New(raftnode.Config{
				DataDir: dir, LocalID: "node-a", BindAddr: "127.0.0.1:0",
				FSM: fsm.New(state.New(brokers), brokers), Keys: testutil.StateKeys(t), LogOutput: io.Discard,
			})
			if err == nil {
				node.Shutdown()
				t.Fatal("historical/incomplete directory was marked as fresh encrypted state")
			}
			if _, err := os.Stat(filepath.Join(dir, "raft", "log.db")); !os.IsNotExist(err) {
				t.Fatalf("failed preflight created a database: %v", err)
			}
		})
	}
}

func TestStartupRejectsWrongKeyAndTamperedLog(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-key", true: "tampered-log"}[tamper], func(t *testing.T) {
			dir := t.TempDir()
			raftDir := filepath.Join(dir, "raft")
			if err := os.Mkdir(raftDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(raftDir, "log.db")
			store, err := boltdb.NewBoltStore(path)
			if err != nil {
				t.Fatal(err)
			}
			keys := testutil.StateKeys(t)
			if err := keys.MarkFreshStore(store); err != nil {
				t.Fatal(err)
			}
			record, err := keys.SealLogFile(&hraft.Log{Index: 1, Term: 1, Type: hraft.LogNoop})
			if err != nil {
				t.Fatal(err)
			}
			if tamper {
				record.Data[len(record.Data)-1] ^= 1
			} else {
				keys, err = seal.New("test-only", map[string][]byte{"test-only": bytes.Repeat([]byte{9}, 32)})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := store.StoreLog(record); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			node, err := raftnode.New(raftnode.Config{
				DataDir: dir, LocalID: "node-a", BindAddr: "127.0.0.1:0",
				FSM: noopFSM{}, Keys: keys, LogOutput: io.Discard,
			})
			if node != nil {
				node.Shutdown()
			}
			if err == nil || !strings.Contains(err.Error(), "authenticate") {
				t.Fatalf("startup did not fail at the authentication boundary: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed startup modified original state")
			}
		})
	}
}
