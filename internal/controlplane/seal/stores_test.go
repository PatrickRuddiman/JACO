package seal_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

func TestProtectedStoresRejectPlaintextBeforePersistence(t *testing.T) {
	keys, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rawLogs := hraft.NewInmemStore()
	logs, err := seal.WrapLogs(rawLogs, keys)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("synthetic-replication-secret")
	if err := logs.StoreLog(&hraft.Log{Index: 1, Type: hraft.LogCommand, Data: marker}); err == nil {
		t.Fatal("accepted plaintext replicated command")
	}
	var entry hraft.Log
	if err := rawLogs.GetLog(1, &entry); !errors.Is(err, hraft.ErrLogNotFound) {
		t.Fatal("rejected plaintext was nevertheless persisted")
	}
	encrypted, err := keys.Seal(seal.CommandPurpose, marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.StoreLog(&hraft.Log{Index: 2, Type: hraft.LogCommand, Data: encrypted}); err != nil {
		t.Fatal(err)
	}
	if err := logs.GetLog(2, &entry); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(entry.Data, encrypted) {
		t.Fatal("log reads must retain ciphertext for replication")
	}

	dir := t.TempDir()
	rawSnapshots, err := hraft.NewFileSnapshotStore(dir, 3, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := seal.WrapSnapshots(rawSnapshots, keys)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := hraft.NewInmemTransport("node-a")
	defer transport.Close()
	sink, err := snapshots.Create(hraft.SnapshotVersionMax, 1, 1, hraft.Configuration{}, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(marker); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err == nil {
		t.Fatal("accepted plaintext installed snapshot")
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, marker) {
			t.Errorf("rejected snapshot leaked to %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	encryptedSnapshot, err := keys.Seal(seal.SnapshotPurpose, marker)
	if err != nil {
		t.Fatal(err)
	}
	sink, err = snapshots.Create(hraft.SnapshotVersionMax, 2, 1, hraft.Configuration{}, 2, transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(encryptedSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	_, reader, err := snapshots.Open(sink.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, encryptedSnapshot) {
		t.Fatal("snapshot reads must retain ciphertext for replication and backup")
	}
}

func TestProtectedStoresRefuseUnmigratedHistory(t *testing.T) {
	keys, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rawLogs := hraft.NewInmemStore()
	if err := rawLogs.StoreLog(&hraft.Log{Index: 1, Type: hraft.LogCommand, Data: []byte("legacy-secret")}); err != nil {
		t.Fatal(err)
	}
	encrypted, err := keys.Seal(seal.CommandPurpose, []byte("new-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rawLogs.StoreLog(&hraft.Log{Index: 2, Type: hraft.LogCommand, Data: encrypted}); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.WrapLogs(rawLogs, keys); err == nil {
		t.Error("startup accepted plaintext history behind a newer encrypted entry")
	}
	rawSnapshots, err := hraft.NewFileSnapshotStore(t.TempDir(), 3, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := hraft.NewInmemTransport("node-a")
	defer transport.Close()
	for i := uint64(1); i <= 2; i++ {
		payload := []byte("legacy-snapshot-secret")
		if i == 2 {
			payload, err = keys.Seal(seal.SnapshotPurpose, payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		sink, err := rawSnapshots.Create(hraft.SnapshotVersionMax, i, 1, hraft.Configuration{}, i, transport)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sink.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seal.WrapSnapshots(rawSnapshots, keys); err == nil {
		t.Error("startup accepted plaintext historical snapshot behind a newer encrypted one")
	}
}
