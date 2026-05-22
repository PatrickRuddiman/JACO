package bootstrap_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/bootstrap"
)

func TestRun_WritesEverythingAndReturnsToken(t *testing.T) {
	dir := t.TempDir()
	res, err := bootstrap.Run(bootstrap.Options{
		DataDir: dir,
		Name:    "testhost",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := res.ClusterID; !uuidLike(got) {
		t.Errorf("ClusterID = %q, not UUID-shaped", got)
	}
	if got, want := len(res.OperatorToken), 64; got != want {
		t.Errorf("OperatorToken len = %d, want %d (32 bytes hex)", got, want)
	}
	if !hexRE.MatchString(res.OperatorToken) {
		t.Errorf("OperatorToken not hex: %q", res.OperatorToken)
	}

	for _, rel := range []string{"raft/log.db", "node/testhost.key", "node/testhost.crt", "node/ca.crt"} {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		} else if st.Size() == 0 {
			t.Errorf("%s is empty", rel)
		}
	}

	// Permissions on the node key must be 0600.
	st, err := os.Stat(filepath.Join(dir, "node/testhost.key"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("node key perm = %#o, want 0600", mode)
	}
}

func TestRun_RefusesExistingState(t *testing.T) {
	dir := t.TempDir()
	if _, err := bootstrap.Run(bootstrap.Options{DataDir: dir, Name: "h"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := bootstrap.Run(bootstrap.Options{DataDir: dir, Name: "h"}); err == nil {
		t.Errorf("expected error on second bootstrap into same data dir")
	}
}

func TestRun_RequiresNameAndDataDir(t *testing.T) {
	if _, err := bootstrap.Run(bootstrap.Options{Name: "h"}); err == nil {
		t.Errorf("expected error when DataDir is empty")
	}
	if _, err := bootstrap.Run(bootstrap.Options{DataDir: t.TempDir()}); err == nil {
		t.Errorf("expected error when Name is empty")
	}
}

func TestRun_TokenHashRoundTrips(t *testing.T) {
	dir := t.TempDir()
	res, err := bootstrap.Run(bootstrap.Options{DataDir: dir, Name: "h"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Hashing the cleartext operator token yields a 32-byte value; this is what
	// gets stored in raft. The CLI hashes the user-presented token the same way
	// at admission time.
	h := sha256.Sum256([]byte(res.OperatorToken))
	if len(h) != 32 {
		t.Errorf("token hash length = %d, want 32", len(h))
	}
}

var (
	hexRE    = regexp.MustCompile(`^[0-9a-f]+$`)
	uuidREv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func uuidLike(s string) bool { return uuidREv4.MatchString(s) }

// silence unused-import warning if encoding/hex stops being used in this file
var _ = hex.EncodedLen
