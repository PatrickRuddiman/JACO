package seal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestSnapshotInventoryDoesNotSkipOrphans(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "snapshots", "1-1-1.tmp")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "state.bin"), []byte("synthetic-orphan-private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seal.ValidateSnapshots(dir, testutil.StateKeys(t)); err == nil {
		t.Fatal("snapshot inventory silently skipped a plaintext temporary directory")
	}
	if _, err := os.Stat(filepath.Join(orphan, "state.bin")); err != nil {
		t.Fatal("snapshot inventory changed original artifacts")
	}
}
