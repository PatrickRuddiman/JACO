// Package storage implements the certmagic.Storage interface fully
// backed by raft: Lock / Unlock through the Cert entity, blobs (Store /
// Load / Delete / List / Stat) through the CertBlob entity (task 40).
//
// The interface JacoStorage matches certmagic.Storage and caddy.Storage
// shape-for-shape so the daemon-side ingress can register it as the
// "jaco" storage module without further adaptation.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/state"
	pb "github.com/PatrickRuddiman/jaco/pkg/proto/jaco/v1"
)

// LockTTL is how long a Lock holds before the FSM accepts a new lessee
// (assuming the holder hasn't called Unlock or auto-renewed).
const LockTTL = 5 * time.Minute

// RenewInterval is the auto-renew cadence the Lock-holder goroutine uses.
const RenewInterval = 2 * time.Minute

// ErrLockHeld is returned by Lock when the lock is currently owned by a
// different lessee and hasn't expired.
var ErrLockHeld = errors.New("lock_held: cert lock owned by another lessee")

// Applier wraps raft.Apply.
type Applier func(cmd []byte) error

// Clock abstracts time.Now so lock-expiry tests can pin time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns the production Clock.
func SystemClock() Clock { return systemClock{} }

// KeyInfo mirrors certmagic.KeyInfo / caddy.KeyInfo.
type KeyInfo struct {
	Key        string
	Modified   time.Time
	Size       int64
	IsTerminal bool
}

// Storage is the public interface JacoStorage satisfies. It mirrors the
// upstream certmagic.Storage interface shape-for-shape so the daemon can
// register *JacoStorage as the "jaco" Caddy storage module.
type Storage interface {
	Store(ctx context.Context, key string, value []byte) error
	Load(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) bool
	List(ctx context.Context, prefix string, recursive bool) ([]string, error)
	Stat(ctx context.Context, key string) (KeyInfo, error)
	Lock(ctx context.Context, name string) error
	Unlock(ctx context.Context, name string) error
}

// JacoStorage implements Storage entirely through raft. Lock / Unlock
// flow through the Cert entity; blobs flow through the CertBlob entity
// added in task 40 — every node sees the same blob set via state.
type JacoStorage struct {
	state  *state.State
	apply  Applier
	clock  Clock
	lessee string

	// renewers tracks active auto-renew goroutines keyed by lock name.
	renewersMu sync.Mutex
	renewers   map[string]context.CancelFunc
}

// New constructs a JacoStorage. lessee is the local node's hostname (used
// as the lock identity in raft); clock may be nil for SystemClock.
func New(st *state.State, apply Applier, lessee string, clock Clock) *JacoStorage {
	if clock == nil {
		clock = SystemClock()
	}
	return &JacoStorage{
		state:    st,
		apply:    apply,
		clock:    clock,
		lessee:   lessee,
		renewers: map[string]context.CancelFunc{},
	}
}

// --- Lock / Unlock --------------------------------------------------------

// Lock acquires the named lock cluster-wide via raft. Returns nil on
// success, ErrLockHeld when another lessee owns the lock + hasn't expired.
// Spawns an auto-renew goroutine that re-applies CertLock every
// RenewInterval until Unlock fires.
func (s *JacoStorage) Lock(ctx context.Context, name string) error {
	if err := s.applyLock(name); err != nil {
		return err
	}
	// Verify acquisition by reading the persisted lessee.
	c, ok := s.state.Certs.Get(name)
	if !ok || c.GetLessee() != s.lessee {
		return ErrLockHeld
	}
	// Start auto-renew.
	s.startRenewer(ctx, name)
	return nil
}

// Unlock releases the lock by raft-Applying CertUnlock. Stops the renewer.
func (s *JacoStorage) Unlock(_ context.Context, name string) error {
	s.stopRenewer(name)
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(s.clock.Now()),
		Payload: &pb.Command_CertUnlock{CertUnlock: &pb.CertUnlock{Name: name}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return s.apply(data)
}

func (s *JacoStorage) applyLock(name string) error {
	now := s.clock.Now()
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(now),
		Payload: &pb.Command_CertLock{CertLock: &pb.CertLock{
			Name:   name,
			Lessee: s.lessee,
			Until:  timestamppb.New(now.Add(LockTTL)),
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return s.apply(data)
}

func (s *JacoStorage) startRenewer(parent context.Context, name string) {
	s.renewersMu.Lock()
	if cancel, ok := s.renewers[name]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.renewers[name] = cancel
	s.renewersMu.Unlock()

	go s.renewLoop(ctx, name)
}

func (s *JacoStorage) stopRenewer(name string) {
	s.renewersMu.Lock()
	defer s.renewersMu.Unlock()
	if cancel, ok := s.renewers[name]; ok {
		cancel()
		delete(s.renewers, name)
	}
}

func (s *JacoStorage) renewLoop(ctx context.Context, name string) {
	t := time.NewTicker(RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.applyLock(name)
		}
	}
}

// --- Store / Load / Delete / Exists / List / Stat ------------------------

// Store raft-Applies CertBlobUpsert{key, value}. The FSM writes the blob
// into state.CertBlobs on every node, so Load on any peer sees the
// payload after replication catches up.
func (s *JacoStorage) Store(_ context.Context, key string, value []byte) error {
	if key == "" {
		return fmt.Errorf("Store: key is required")
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(s.clock.Now()),
		Payload: &pb.Command_CertBlobUpsert{CertBlobUpsert: &pb.CertBlobUpsert{
			Blob: &pb.CertBlob{
				Key:       key,
				Value:     cp,
				UpdatedAt: timestamppb.New(s.clock.Now()),
			},
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return s.apply(data)
}

// Load reads the value for key from state.CertBlobs. Returns ErrNotExist
// when the key is absent (certmagic checks errors.Is(err, fs.ErrNotExist)).
func (s *JacoStorage) Load(_ context.Context, key string) ([]byte, error) {
	b, ok := s.state.CertBlobs.Get(key)
	if !ok {
		return nil, fmt.Errorf("Load %s: %w", key, ErrNotExist)
	}
	cp := make([]byte, len(b.GetValue()))
	copy(cp, b.GetValue())
	return cp, nil
}

// Delete raft-Applies CertBlobRemove{key}. No-op when absent (matches
// certmagic's contract).
func (s *JacoStorage) Delete(_ context.Context, key string) error {
	cmd := &pb.Command{
		Identity: "ingress",
		Ts:       timestamppb.New(s.clock.Now()),
		Payload:  &pb.Command_CertBlobRemove{CertBlobRemove: &pb.CertBlobRemove{Key: key}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return s.apply(data)
}

// Exists reports whether key has a value in state.CertBlobs.
func (s *JacoStorage) Exists(_ context.Context, key string) bool {
	_, ok := s.state.CertBlobs.Get(key)
	return ok
}

// List returns the keys under prefix. When recursive=false, returns direct
// children only; when true, returns the full path of every descendant.
func (s *JacoStorage) List(_ context.Context, prefix string, recursive bool) ([]string, error) {
	seen := map[string]bool{}
	for _, b := range s.state.CertBlobs.List() {
		k := b.GetKey()
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(k, prefix)
		remainder = strings.TrimPrefix(remainder, "/")
		if remainder == "" {
			continue
		}
		if recursive {
			seen[k] = true
			continue
		}
		if idx := strings.IndexByte(remainder, '/'); idx >= 0 {
			remainder = remainder[:idx]
		}
		var full string
		if prefix == "" {
			full = remainder
		} else if strings.HasSuffix(prefix, "/") {
			full = prefix + remainder
		} else {
			full = prefix + "/" + remainder
		}
		seen[full] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Stat returns metadata for key. IsTerminal=true reflects that the key has
// a value (vs. being just a directory prefix).
func (s *JacoStorage) Stat(_ context.Context, key string) (KeyInfo, error) {
	b, ok := s.state.CertBlobs.Get(key)
	if !ok {
		return KeyInfo{}, fmt.Errorf("Stat %s: %w", key, ErrNotExist)
	}
	return KeyInfo{
		Key:        key,
		Modified:   b.GetUpdatedAt().AsTime(),
		Size:       int64(len(b.GetValue())),
		IsTerminal: true,
	}, nil
}

// ErrNotExist is the sentinel Load/Stat return for missing keys. certmagic
// decides "no such cert/key yet" via errors.Is(err, fs.ErrNotExist), so it
// MUST wrap fs.ErrNotExist — otherwise certmagic treats a missing blob as a
// hard error and ACME issuance/loading breaks. errors.New (no wrap) was the
// latent bug; %w fixes matching while keeping a JACO-specific message.
var ErrNotExist = fmt.Errorf("key does not exist: %w", fs.ErrNotExist)
