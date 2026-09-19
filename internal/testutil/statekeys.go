package testutil

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

// StateKeys supplies synthetic, repeatable keys to test replicas and restarts.
// Production constructors never generate or default a wrapping key.
func StateKeys(t testing.TB) *seal.Keyring {
	t.Helper()
	key := sha256.Sum256([]byte("JACO synthetic test-only state wrapping key"))
	keys, err := seal.New("test-only", map[string][]byte{"test-only": key[:]})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func StateKeyFile(t testing.TB) string {
	t.Helper()
	key := sha256.Sum256([]byte("JACO synthetic test-only state wrapping key"))
	data, err := json.Marshal(struct {
		Version int               `json:"version"`
		Active  string            `json:"active"`
		Keys    map[string][]byte `json:"keys"`
	}{1, "test-only", map[string][]byte{"test-only": key[:]}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state-keys.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
