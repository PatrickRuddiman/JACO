package seal_test

import (
	"bytes"
	"errors"
	"os"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
)

type failingStableStore struct{ hraft.StableStore }

func (failingStableStore) Get([]byte) ([]byte, error) { return nil, os.ErrPermission }

func TestProvenanceBindsCompleteRing(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old, err := seal.New("old", map[string][]byte{"old": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := seal.New("new", map[string][]byte{"old": oldKey, "new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	store := hraft.NewInmemStore()
	if err := old.MarkFreshStore(store); err != nil {
		t.Fatal(err)
	}
	if err := changed.CheckStore(store); err == nil {
		t.Fatal("startup accepted an uncoordinated keyring change")
	}
	if err := old.CheckStore(failingStableStore{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("store I/O failure was masked as legacy state: %v", err)
	}
}
