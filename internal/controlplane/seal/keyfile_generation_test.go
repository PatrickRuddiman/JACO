package seal_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

func TestKeyFileGenerationAndRetainedRotation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production key files require POSIX owner-only permissions")
	}
	dataDir, keyDir := t.TempDir(), t.TempDir()
	first := filepath.Join(keyDir, "first.json")
	if err := seal.GenerateFile(seal.KeyFileOptions{Path: first, DataDir: dataDir, KeyID: "one"}); err != nil {
		t.Fatal(err)
	}
	old, err := seal.LoadFile(first, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := old.Seal("test", []byte("synthetic-rotation-secret"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := seal.GenerateFile(seal.KeyFileOptions{Path: first, DataDir: dataDir, KeyID: "two"}); err == nil {
		t.Fatal("existing recovery key file overwritten")
	}
	after, err := os.ReadFile(first)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing key file changed")
	}
	second := filepath.Join(keyDir, "second.json")
	if err := seal.GenerateFile(seal.KeyFileOptions{Path: second, DataDir: dataDir, KeyID: "two", RetainFile: first}); err != nil {
		t.Fatal(err)
	}
	rotated, err := seal.LoadFile(second, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := rotated.Open("test", value); err != nil || string(plain) != "synthetic-rotation-secret" {
		t.Fatal("rotation stranded historical data")
	}
	t.Setenv("JACO_STATE_KEY_FILE", second)
	if _, err := seal.LoadFile("", dataDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JACO_STATE_KEY_FILE", "")
	credential := filepath.Join(keyDir, "jaco-state-keys")
	if err := os.WriteFile(credential, before, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", keyDir)
	if _, err := seal.LoadFile("", dataDir); err != nil {
		t.Fatalf("service-manager credential did not load: %v", err)
	}
}

func TestKeyGenerationRefusesDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "wrapping-key.json")
	if err := seal.GenerateFile(seal.KeyFileOptions{Path: path, DataDir: dataDir, KeyID: "one"}); err == nil {
		t.Fatal("wrapping key generated inside exported data")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unsafe provisioning created a key file")
	}
}
