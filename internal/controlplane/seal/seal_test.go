package seal_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	keys, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("synthetic-compose-registry-ca-acme-secret")
	encrypted, err := keys.Seal("raft-command", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, secret) {
		t.Fatal("envelope contains plaintext")
	}
	plain, err := keys.Open("raft-command", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, secret) {
		t.Fatal("authorized decryption changed the payload")
	}
	again, err := keys.Seal("raft-command", secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again, encrypted) {
		t.Fatal("repeated encryption reused an envelope")
	}
}

func TestEnvelopeRejectsUnauthenticatedPayloads(t *testing.T) {
	keys, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := keys.Seal("raft-command", []byte("synthetic-private-key"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range encrypted {
		corrupt := bytes.Clone(encrypted)
		corrupt[i] ^= 1
		if _, err := keys.Open("raft-command", corrupt); err == nil {
			t.Fatalf("accepted corruption at byte %d", i)
		}
	}
	for i := range encrypted {
		if _, err := keys.Open("raft-command", encrypted[:i]); err == nil {
			t.Fatalf("accepted truncation at byte %d", i)
		}
	}
	if _, err := keys.Open("raft-snapshot", encrypted); err == nil {
		t.Fatal("accepted command as snapshot")
	}
	wrong, err := seal.New("current", map[string][]byte{"current": bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Open("raft-command", encrypted); err == nil {
		t.Fatal("accepted wrong key with matching ID")
	}
	if _, err := keys.Open("raft-command", []byte("legacy-plaintext")); err == nil {
		t.Fatal("accepted plaintext")
	}
}

func TestLoadExternalKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon key files require POSIX permissions; in-memory keyrings are portable")
	}
	dataDir := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "state-keys.json")
	body, err := json.Marshal(map[string]any{
		"version": 1, "active": "current",
		"keys": map[string][]byte{"current": bytes.Repeat([]byte{9}, 32)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := seal.LoadFile(keyFile, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal("raft-command", []byte("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := seal.LoadFile(keyFile, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Open("raft-command", sealed); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileRejectsUnsafeProvisioning(t *testing.T) {
	dataDir := t.TempDir()
	inside := filepath.Join(dataDir, "key.json")
	if err := os.WriteFile(inside, []byte("synthetic-invalid-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.LoadFile(inside, dataDir); err == nil {
		t.Fatal("accepted a wrapping key inside the data directory")
	}
	if _, err := seal.LoadFile("relative-key.json", dataDir); err == nil {
		t.Fatal("accepted a relative key path")
	}
	shared := filepath.Join(t.TempDir(), "shared-key.json")
	if err := os.WriteFile(shared, []byte("synthetic-invalid-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.LoadFile(shared, dataDir); err == nil {
		t.Fatal("accepted a group/world-readable wrapping key")
	}
	link := filepath.Join(t.TempDir(), "linked-key.json")
	if err := os.Symlink(shared, link); err == nil {
		if _, err := seal.LoadFile(link, dataDir); err == nil {
			t.Fatal("accepted a symlink key file")
		}
	}
}

func TestRotationRetainsOldRecoveryKeysUntilReencryption(t *testing.T) {
	oldMaterial := bytes.Repeat([]byte{7}, 32)
	newMaterial := bytes.Repeat([]byte{8}, 32)
	old, err := seal.New("old", map[string][]byte{"old": oldMaterial})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := old.Seal("raft-snapshot", []byte("synthetic-recovery-secret"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := seal.New("new", map[string][]byte{"old": oldMaterial, "new": newMaterial})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rotated.Open("raft-snapshot", backup)
	if err != nil {
		t.Fatal("retaining the old key must keep historical backups recoverable")
	}
	replacement, err := rotated.Seal("raft-snapshot", plain)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := seal.New("new", map[string][]byte{"new": newMaterial})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retired.Open("raft-snapshot", backup); err == nil {
		t.Fatal("old backup unexpectedly decrypts after old key retirement")
	}
	recovered, err := retired.Open("raft-snapshot", replacement)
	if err != nil || !bytes.Equal(recovered, plain) {
		t.Fatal("re-encrypted backup is not recoverable with the new key alone")
	}
}
