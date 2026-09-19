package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/seal"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	cache "github.com/PatrickRuddiman/jaco/internal/ingress/storage"
	"github.com/PatrickRuddiman/jaco/internal/testutil"
)

func TestDiskCacheEncryptsPrivateMaterial(t *testing.T) {
	dir := t.TempDir()
	storage, _, _ := newHarnessWithCache(t, "node-a", dir)
	marker := []byte("synthetic-ACME-account-private-key")
	key := "acme/accounts/operator@example.test/private.key"
	if err := storage.Store(context.Background(), key, marker); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one cache artifact, got %d", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, marker) {
		t.Fatal("ACME fallback cache contains plaintext key material")
	}
	restarted, _, _ := newHarnessWithCache(t, "node-a", dir)
	plain, err := restarted.Load(context.Background(), key)
	if err != nil || !bytes.Equal(plain, marker) {
		t.Fatalf("authorized fallback after restart failed: %v", err)
	}
}

func TestCacheAuthFailureIsNotAHiddenMiss(t *testing.T) {
	dir := t.TempDir()
	writer, _, _ := newHarnessWithCache(t, "node-a", dir)
	key := "account/private.key"
	if err := writer.Store(context.Background(), key, []byte("synthetic-account-secret")); err != nil {
		t.Fatal(err)
	}
	wrong, err := seal.New("test-only", map[string][]byte{"test-only": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	brokers := watch.NewRegistry()
	reader := cache.NewWithCache(state.New(brokers), func([]byte) error { return nil }, "node-a", nil, dir, wrong)
	if _, err := reader.Load(context.Background(), key); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrong-key failure hidden as cache miss: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, entries[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reader = cache.NewWithCache(state.New(brokers), func([]byte) error { return nil }, "node-a", nil, dir, testutil.StateKeys(t))
	if _, err := reader.Stat(context.Background(), key); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tamper failure hidden as cache miss: %v", err)
	}
}
