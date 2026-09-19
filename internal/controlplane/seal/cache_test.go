package seal_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestCacheStartupAuthenticatesAllArtifacts(t *testing.T) {
	keys := testutil.StateKeys(t)
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("account/private.key"))
	name := hex.EncodeToString(sum[:])
	ciphertext, err := keys.Seal(seal.CachePurposePrefix+name, []byte("synthetic-account-key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seal.ValidateCache(dir, keys); err != nil {
		t.Fatal(err)
	}
	wrong, err := seal.New("test-only", map[string][]byte{"test-only": bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := seal.ValidateCache(dir, wrong); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err := os.WriteFile(path+".tmp-123", []byte("synthetic-legacy-orphan-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seal.ValidateCache(dir, keys); err == nil {
		t.Fatal("plaintext orphan cache artifact accepted")
	}
	if raw, err := os.ReadFile(path + ".tmp-123"); err != nil || !bytes.Contains(raw, []byte("legacy")) {
		t.Fatal("startup must retain historical artifacts for explicit migration")
	}
}
