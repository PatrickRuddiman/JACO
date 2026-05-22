// Package state owns the in-memory typed entity stores backing the control
// plane's FSM. Each store is keyed by a single string; mutations publish the
// matching watch event through the broker registry.
//
// State is meant to be mutated only from the FSM Apply path (one goroutine).
// Reads are concurrent-safe via per-store RWMutex.
package state

import (
	"sync"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
	"google.golang.org/protobuf/proto"
)

// Store is a typed in-memory store for one entity type. The key function is
// supplied at construction; the type parameter T must be a proto.Message so we
// can defensively clone reads and writes.
type Store[T proto.Message] struct {
	mu     sync.RWMutex
	byKey  map[string]T
	keyFn  func(T) string
	broker *watch.Broker[T]
}

// NewStore constructs an empty store wired to a broker.
func NewStore[T proto.Message](broker *watch.Broker[T], keyFn func(T) string) *Store[T] {
	return &Store[T]{
		byKey:  make(map[string]T),
		keyFn:  keyFn,
		broker: broker,
	}
}

// Get returns a defensive copy of the entry under key, or zero if absent.
func (s *Store[T]) Get(key string) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.byKey[key]
	if !ok {
		var zero T
		return zero, false
	}
	return clone(v), true
}

// List returns defensive copies of every entry. Order is unspecified.
func (s *Store[T]) List() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, 0, len(s.byKey))
	for _, v := range s.byKey {
		out = append(out, clone(v))
	}
	return out
}

// Len reports the entry count.
func (s *Store[T]) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byKey)
}

// Apply upserts v and publishes the matching watch event. Returns true if the
// entry was newly added (vs. updated).
func (s *Store[T]) Apply(v T, raftIndex uint64) bool {
	key := s.keyFn(v)
	stored := clone(v)

	s.mu.Lock()
	before, existed := s.byKey[key]
	s.byKey[key] = stored
	s.mu.Unlock()

	if existed {
		s.broker.Publish(watch.Event[T]{
			Kind: watch.KindUpdated, Before: clone(before), After: clone(stored), RaftIndex: raftIndex,
		})
		return false
	}
	s.broker.Publish(watch.Event[T]{
		Kind: watch.KindAdded, After: clone(stored), RaftIndex: raftIndex,
	})
	return true
}

// Remove deletes the entry under key and publishes a Removed event. Returns
// true if anything was removed.
func (s *Store[T]) Remove(key string, raftIndex uint64) bool {
	s.mu.Lock()
	before, existed := s.byKey[key]
	if existed {
		delete(s.byKey, key)
	}
	s.mu.Unlock()
	if !existed {
		return false
	}
	s.broker.Publish(watch.Event[T]{
		Kind: watch.KindRemoved, Before: clone(before), RaftIndex: raftIndex,
	})
	return true
}

func clone[T proto.Message](v T) T {
	return proto.Clone(v).(T)
}
