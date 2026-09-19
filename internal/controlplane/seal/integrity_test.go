package seal_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestFSMAuthenticationFailureIsFatal(t *testing.T) {
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	keys := testutil.StateKeys(t)
	protected, err := seal.WrapFSM(fsm.New(st, brokers), keys)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := keys.Seal(seal.CommandPurpose, []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	defer func() {
		if recover() == nil {
			t.Error("crypto failure returned to Raft, which would advance its applied index")
		}
	}()
	protected.Apply(&hraft.Log{Index: 17, Type: hraft.LogCommand, Data: ciphertext})
}

func TestSnapshotMetadataAuthenticated(t *testing.T) {
	dir := t.TempDir()
	keys := testutil.StateKeys(t)
	raw, err := hraft.NewFileSnapshotStore(dir, 3, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store, err := seal.WrapSnapshots(raw, keys)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := hraft.NewInmemTransport("node-a")
	defer transport.Close()
	sink, err := store.Create(hraft.SnapshotVersionMax, 7, 3, hraft.Configuration{}, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	data, err := keys.Seal(seal.SnapshotPurpose, []byte("synthetic-snapshot-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "snapshots", sink.ID(), "meta.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["Index"] = json.RawMessage("8")
	metadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, reader, err := store.Open(sink.ID()); err == nil {
		reader.Close()
		t.Fatal("accepted snapshot with modified replay index")
	}
}

func TestLogReplayMetadataAuthenticated(t *testing.T) {
	keys := testutil.StateKeys(t)
	raw := hraft.NewInmemStore()
	store, err := seal.WrapLogs(raw, keys)
	if err != nil {
		t.Fatal(err)
	}
	data, err := keys.Seal(seal.CommandPurpose, []byte("synthetic-command-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreLog(&hraft.Log{Index: 7, Term: 3, Type: hraft.LogCommand, Data: data}); err != nil {
		t.Fatal(err)
	}
	var entry hraft.Log
	if err := raw.GetLog(7, &entry); err != nil {
		t.Fatal(err)
	}
	entry.Index = 8
	if err := raw.StoreLog(&entry); err != nil {
		t.Fatal(err)
	}
	if err := store.GetLog(8, &entry); err == nil {
		t.Fatal("accepted an authenticated command moved to a different replay index")
	}
}
