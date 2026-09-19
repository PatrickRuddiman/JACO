package raftnode_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	raftnode "github.com/PatrickRuddiman/jaco/internal/controlplane/raft"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

func TestEncryptedStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	keys := testutil.StateKeys(t)
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	node, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: "127.0.0.1:0", LocalID: "node-a", Bootstrap: true,
		FSM: fsm.New(st, brokers), Keys: keys, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Shutdown()
	if !waitForLeader(t, node, 5*time.Second) {
		t.Fatal("no leader")
	}
	cmd, err := proto.Marshal(&pb.Command{Payload: &pb.Command_DeploymentApply{DeploymentApply: &pb.DeploymentApply{
		Deployment: "app", Revision: 7, ComposeYaml: []byte("synthetic-persisted-compose-secret"),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Apply(cmd, 0); err != nil {
		t.Fatal(err)
	}
	if err := node.Raft.Snapshot().Error(); err != nil {
		t.Fatal(err)
	}
	addr := string(node.LocalAddr())
	if err := node.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("synthetic-persisted-compose-secret")) {
			t.Errorf("durable artifact contains plaintext: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	brokers = watch.NewRegistry()
	restored := state.New(brokers)
	restarted, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: addr, LocalID: "node-a",
		FSM: fsm.New(restored, brokers), Keys: keys, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	if !waitForLeader(t, restarted, 5*time.Second) {
		t.Fatal("restarted node did not elect leader")
	}
	deployment, ok := restored.Deployments.Get("app")
	if !ok || string(deployment.GetComposeYaml()) != "synthetic-persisted-compose-secret" {
		t.Fatal("restart did not recover authorized runtime state")
	}
}

func TestUnmarkedStoreRequiresMigrationEvenWithOnlyFreePageSecrets(t *testing.T) {
	dir := t.TempDir()
	raftDir := filepath.Join(dir, "raft")
	if err := os.MkdirAll(raftDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(raftDir, "log.db")
	store, err := boltdb.NewBoltStore(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("synthetic-retired-compose-secret")
	if err := store.StoreLog(&hraft.Log{Index: 1, Type: hraft.LogCommand, Data: bytes.Repeat(marker, 1024)}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRange(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, marker) {
		t.Fatal("fixture must retain a secret in the old Bolt free pages")
	}
	node, err := raftnode.New(raftnode.Config{
		DataDir: dir, BindAddr: "127.0.0.1:0", LocalID: "node-a",
		FSM: noopFSM{}, Keys: testutil.StateKeys(t), LogOutput: io.Discard,
	})
	if node != nil {
		_ = node.Shutdown()
	}
	if err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("unmarked historical store must require migration, got %v", err)
	}
}
