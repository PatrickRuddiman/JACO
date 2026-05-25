package storage_test

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	hraft "github.com/hashicorp/raft"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/fsm"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"github.com/PatrickRuddiman/jaco/internal/ingress/storage"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newHarness builds state+FSM+applier so the storage's raft writes land in
// state.Certs. lessee is the node identity baked into Lock calls.
func newHarness(t *testing.T, lessee string, clock *fakeClock) (*storage.JacoStorage, *state.State) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	return storage.New(st, apply, lessee, clock.Now), st
}

func TestLock_FirstLesseeAcquires(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	s, st := newHarness(t, "node-a", clock)
	if err := s.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	c, ok := st.Certs.Get("issue_cert_example.com")
	if !ok {
		t.Fatalf("Cert entry missing post-Lock")
	}
	if c.GetLessee() != "node-a" {
		t.Errorf("lessee = %q, want node-a", c.GetLessee())
	}
	// Cleanup the renewer goroutine.
	_ = s.Unlock(context.Background(), "issue_cert_example.com")
}

func TestLock_ContentionResolvedWithSingleWinner(t *testing.T) {
	// The AC: two Lock calls from different lessees against state — one
	// succeeds, the other returns ErrLockHeld. They share state so the
	// second goes through the FSM's reject path.
	clock := newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	sA := storage.New(st, apply, "node-a", clock.Now)
	sB := storage.New(st, apply, "node-b", clock.Now)

	if err := sA.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("node-a Lock: %v", err)
	}
	err := sB.Lock(context.Background(), "issue_cert_example.com")
	if !errors.Is(err, storage.ErrLockHeld) {
		t.Errorf("node-b Lock err = %v; want ErrLockHeld", err)
	}
	// state.Certs still shows node-a as the lessee.
	c, _ := st.Certs.Get("issue_cert_example.com")
	if c.GetLessee() != "node-a" {
		t.Errorf("lessee = %q after contention; want node-a", c.GetLessee())
	}
	_ = sA.Unlock(context.Background(), "issue_cert_example.com")
}

func TestLock_ExpiredLockAcquirableByNewLessee(t *testing.T) {
	// The AC: after LockTTL (5min), a new lessee can take the lock.
	clock := newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	sA := storage.New(st, apply, "node-a", clock.Now)
	sB := storage.New(st, apply, "node-b", clock.Now)

	if err := sA.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	_ = sA.Unlock(context.Background(), "issue_cert_example.com") // stop A's renewer goroutine

	// Re-lock from A (its renewer is dead now), then advance past TTL.
	if err := sA.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("re-Lock: %v", err)
	}
	_ = sA.Unlock(context.Background(), "issue_cert_example.com")

	// Manually re-establish A's lock without starting its renewer (we want
	// to test what happens when the lock expires naturally).
	if err := sA.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("third Lock: %v", err)
	}
	// stop A's renewer goroutine before advancing the clock.
	clock.Advance(storage.LockTTL + time.Second)

	// node-b now applies fresh CertLock at a later cmd.Ts; FSM should
	// accept since the existing lock (until=t+5min) is now in the past.
	if err := sB.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Errorf("node-b should acquire expired lock; got %v", err)
	}
	c, _ := st.Certs.Get("issue_cert_example.com")
	if c.GetLessee() != "node-b" {
		t.Errorf("after expiry, lessee = %q; want node-b", c.GetLessee())
	}
	_ = sB.Unlock(context.Background(), "issue_cert_example.com")
}

func TestUnlock_ReleasesAndAllowsImmediateReacquire(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	sA := storage.New(st, apply, "node-a", clock.Now)
	sB := storage.New(st, apply, "node-b", clock.Now)

	if err := sA.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("Lock A: %v", err)
	}
	if err := sA.Unlock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := sB.Lock(context.Background(), "issue_cert_example.com"); err != nil {
		t.Errorf("Lock B after Unlock: %v; should acquire freely", err)
	}
	_ = sB.Unlock(context.Background(), "issue_cert_example.com")
}

func TestStoreLoad_RoundTrip(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	ctx := context.Background()
	key := "certificates/acme/example.com/example.com.crt"
	val := []byte("-----BEGIN CERTIFICATE-----\nMIIC...\n-----END CERTIFICATE-----\n")
	if err := s.Store(ctx, key, val); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := s.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(val) {
		t.Errorf("round-trip mismatch")
	}
	if !s.Exists(ctx, key) {
		t.Errorf("Exists = false post-Store")
	}
	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != key {
		t.Errorf("Stat key = %q, want %q", info.Key, key)
	}
	if info.Size != int64(len(val)) {
		t.Errorf("Stat size = %d, want %d", info.Size, len(val))
	}
	if !info.IsTerminal {
		t.Errorf("IsTerminal = false")
	}
}

func TestLoad_MissingKeyReturnsNotExist(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	_, err := s.Load(context.Background(), "no/such/key")
	if !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("Load err = %v; want ErrNotExist", err)
	}
	// certmagic decides "no cert yet" via errors.Is(err, fs.ErrNotExist), so the
	// missing-key error MUST also match fs.ErrNotExist or ACME issuance breaks.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load err = %v; must match fs.ErrNotExist for certmagic", err)
	}
}

func TestCertMagicStorage_ConvertsStatAndIsUsable(t *testing.T) {
	// caddy.StorageConverter: the registered module must hand back a working
	// certmagic.Storage whose Stat returns certmagic.KeyInfo (the missing
	// method that panicked the TLS automation policy, issue #28).
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	cm, err := s.CertMagicStorage()
	if err != nil {
		t.Fatalf("CertMagicStorage: %v", err)
	}
	if cm == nil {
		t.Fatal("CertMagicStorage returned nil certmagic.Storage")
	}
	ctx := context.Background()
	if err := s.Store(ctx, "acme/site.crt", []byte("hello")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	ki, err := cm.Stat(ctx, "acme/site.crt")
	if err != nil {
		t.Fatalf("certmagic Stat: %v", err)
	}
	if ki.Key != "acme/site.crt" || ki.Size != 5 || !ki.IsTerminal {
		t.Errorf("certmagic.KeyInfo = %+v, want {Key:acme/site.crt Size:5 IsTerminal:true}", ki)
	}
	if _, err := cm.Stat(ctx, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("certmagic Stat missing err = %v; want fs.ErrNotExist", err)
	}
}

func TestDelete_RemovesKey(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	ctx := context.Background()
	_ = s.Store(ctx, "key", []byte("v"))
	if !s.Exists(ctx, "key") {
		t.Fatalf("preconditions")
	}
	if err := s.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Exists(ctx, "key") {
		t.Errorf("Exists = true after Delete")
	}
}

func TestList_NonRecursiveReturnsDirectChildren(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	ctx := context.Background()
	keys := []string{
		"acme/example.com/cert.pem",
		"acme/example.com/key.pem",
		"acme/other.com/cert.pem",
		"acme/foo/bar/cert.pem",
	}
	for _, k := range keys {
		_ = s.Store(ctx, k, []byte("v"))
	}
	got, err := s.List(ctx, "acme", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"acme/example.com": true, "acme/other.com": true, "acme/foo": true}
	if len(got) != len(want) {
		t.Errorf("List len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
}

func TestList_RecursiveReturnsFullPaths(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	ctx := context.Background()
	keys := []string{
		"acme/example.com/cert.pem",
		"acme/example.com/key.pem",
		"acme/other.com/cert.pem",
	}
	for _, k := range keys {
		_ = s.Store(ctx, k, []byte("v"))
	}
	got, err := s.List(ctx, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("List recursive = %v, want 3 entries", got)
	}
}

func TestStat_MissingKeyReturnsNotExist(t *testing.T) {
	s, _ := newHarness(t, "node-a", newFakeClock(time.Now()))
	_, err := s.Stat(context.Background(), "missing")
	if !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("Stat missing err = %v; want ErrNotExist", err)
	}
}

// newHarnessWithCache builds state+FSM+applier and a JacoStorage with the
// disk fallback cache rooted at cacheDir.
func newHarnessWithCache(t *testing.T, lessee, cacheDir string) (*storage.JacoStorage, *state.State, storage.Applier) {
	t.Helper()
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	f := fsm.New(st, brokers)
	var raftIdx uint64
	apply := func(data []byte) error {
		raftIdx++
		f.Apply(&hraft.Log{Index: raftIdx, Data: data})
		return nil
	}
	return storage.NewWithCache(st, apply, lessee, time.Now, cacheDir), st, apply
}

// TestDiskCache_SurvivesRaftWipe — the disk fallback (issue #41) serves an
// already-issued cert after the raft state is wiped, avoiding re-issuance.
func TestDiskCache_SurvivesRaftWipe(t *testing.T) {
	dir := t.TempDir()
	s1, _, _ := newHarnessWithCache(t, "node-a", dir)
	ctx := context.Background()
	key := "certificates/acme-v02.api.letsencrypt.org-directory/example.com/example.com.crt"
	val := []byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n")
	if err := s1.Store(ctx, key, val); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Simulate a raft wipe: brand-new state (empty CertBlobs) pointed at the
	// SAME disk cache dir.
	s2, st2, _ := newHarnessWithCache(t, "node-a", dir)
	if _, ok := st2.CertBlobs.Get(key); ok {
		t.Fatal("precondition: fresh state should have no raft blob")
	}
	got, err := s2.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load after raft wipe (should hit disk): %v", err)
	}
	if string(got) != string(val) {
		t.Errorf("disk-fallback Load = %q, want %q", got, val)
	}
	if !s2.Exists(ctx, key) {
		t.Errorf("Exists = false after raft wipe; disk fallback should report true")
	}
	if _, err := s2.Stat(ctx, key); err != nil {
		t.Errorf("Stat after raft wipe: %v", err)
	}
}

// TestDiskCache_LoadReseedsRaft — issue #65: when Load serves a cert from the
// disk fallback (raft wiped but the cache survived), it must re-seed raft so
// peers — which can only read replicated CertBlobs, never this node's local
// disk — can load it too. Otherwise the leader serves from disk while every
// follower fails TLS.
func TestDiskCache_LoadReseedsRaft(t *testing.T) {
	dir := t.TempDir()
	s1, _, _ := newHarnessWithCache(t, "node-a", dir)
	ctx := context.Background()
	key := "certificates/x/peer.example.com/peer.example.com.crt"
	val := []byte("cert-bytes")
	if err := s1.Store(ctx, key, val); err != nil {
		t.Fatal(err)
	}

	// Fresh raft state (wipe) + same disk dir.
	s2, st2, _ := newHarnessWithCache(t, "node-a", dir)
	if _, ok := st2.CertBlobs.Get(key); ok {
		t.Fatal("precondition: fresh state should have no raft blob")
	}
	if _, err := s2.Load(ctx, key); err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := st2.CertBlobs.Get(key)
	if !ok {
		t.Fatal("disk-fallback Load did not re-seed raft CertBlobs (issue #65)")
	}
	if string(b.GetValue()) != string(val) {
		t.Errorf("re-seeded blob = %q, want %q", b.GetValue(), val)
	}
}

// TestDiskCache_LoadReseedBestEffort — on a follower the re-seed Apply fails
// (not leader); Load must still serve the cached value without surfacing the
// error.
func TestDiskCache_LoadReseedBestEffort(t *testing.T) {
	dir := t.TempDir()
	seed, _, _ := newHarnessWithCache(t, "node-a", dir)
	ctx := context.Background()
	key := "certificates/x/follower.example.com/follower.example.com.crt"
	val := []byte("cert-bytes")
	if err := seed.Store(ctx, key, val); err != nil {
		t.Fatal(err)
	}

	// Follower: fresh raft state, same disk dir, an Apply that always fails.
	brokers := watch.NewRegistry()
	st := state.New(brokers)
	follower := storage.NewWithCache(st, func([]byte) error {
		return errors.New("node is not the leader")
	}, "node-b", time.Now, dir)
	got, err := follower.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load on follower (apply fails) should still serve disk value: %v", err)
	}
	if string(got) != string(val) {
		t.Errorf("Load = %q, want %q", got, val)
	}
}

// TestDiskCache_RaftWins — when raft has the blob, Load returns the raft copy
// (raft is authoritative); the disk cache never overrides it.
func TestDiskCache_RaftWins(t *testing.T) {
	dir := t.TempDir()
	s, _, _ := newHarnessWithCache(t, "node-a", dir)
	ctx := context.Background()
	key := "certificates/x/example.com/example.com.crt"
	if err := s.Store(ctx, key, []byte("raft-value")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "raft-value" {
		t.Errorf("Load = %q, want raft-value (raft authoritative)", got)
	}
}

// TestDiskCache_DeleteRemovesMirror — Delete drops both the raft blob and the
// disk mirror.
func TestDiskCache_DeleteRemovesMirror(t *testing.T) {
	dir := t.TempDir()
	s1, _, _ := newHarnessWithCache(t, "node-a", dir)
	ctx := context.Background()
	key := "certificates/x/d.example.com/d.example.com.crt"
	_ = s1.Store(ctx, key, []byte("v"))
	if err := s1.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	// Fresh raft state + same disk dir: the mirror must be gone too.
	s2, _, _ := newHarnessWithCache(t, "node-a", dir)
	if _, err := s2.Load(ctx, key); !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("Load after Delete = %v; want ErrNotExist (mirror not removed)", err)
	}
}

// TestDiskCache_DisabledWhenNoDir — New() (cacheDir="") never writes to disk;
// a fresh raft state with no disk fallback returns ErrNotExist on Load.
func TestDiskCache_DisabledWhenNoDir(t *testing.T) {
	s1, _, _ := newHarnessWithCache(t, "node-a", "") // cache off
	ctx := context.Background()
	key := "certificates/x/nodisk.example.com/nodisk.example.com.crt"
	if err := s1.Store(ctx, key, []byte("v")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	// Fresh raft state, cache still off: no disk fallback to seed from.
	s2, _, _ := newHarnessWithCache(t, "node-a", "")
	if _, err := s2.Load(ctx, key); !errors.Is(err, storage.ErrNotExist) {
		t.Errorf("Load with cache disabled = %v; want ErrNotExist", err)
	}
}
