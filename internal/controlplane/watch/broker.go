// Package watch implements bounded-buffer pub/sub brokers used by the control
// plane to fan typed mutations out to in-process subscribers (scheduler,
// runtime, ingress, discovery, audit tail, status -w).
//
// Each entity type gets its own *Broker[T] in the Registry. Subscribers pull
// from the returned channel. On overflow the broker drops the oldest queued
// event and emits a synthetic Event{Kind: KindResync} so the subscriber knows
// to re-fetch the entity store from scratch (see control-plane slice §5).
package watch

import "sync"

// DefaultBuffer is the per-subscriber channel buffer used by Broker.Subscribe
// when no override is requested. Sized for steady-state bursts during rolling
// updates without backing up Apply.
const DefaultBuffer = 256

// Kind tags every event with the mutation type or the synthetic Resync.
type Kind int

const (
	KindUnspecified Kind = 0
	KindAdded       Kind = 1
	KindUpdated     Kind = 2
	KindRemoved     Kind = 3
	KindResync      Kind = 4
)

// Event[T] is the per-entity event payload. Before and After are zero values
// for KindAdded (no Before), KindRemoved (no After), and KindResync (both).
type Event[T any] struct {
	Kind      Kind
	Before    T
	After     T
	RaftIndex uint64
}

// Subscription is a handle to a live subscription. Events returns the receive
// channel; Cancel removes the subscription from the broker and closes the
// channel exactly once.
type Subscription[T any] struct {
	ch     chan Event[T]
	cancel func()
}

// Events returns the receive-only event channel.
func (s *Subscription[T]) Events() <-chan Event[T] { return s.ch }

// Cancel unsubscribes and closes the event channel. Idempotent.
func (s *Subscription[T]) Cancel() { s.cancel() }

// Broker is a single-type pub/sub broker. Publishers call Publish; subscribers
// call Subscribe and read Events. Publish never blocks: on overflow the oldest
// in-flight event is dropped and a Resync is enqueued in its place.
type Broker[T any] struct {
	mu          sync.Mutex
	subscribers map[*subscriberState[T]]struct{}
	buffer      int
}

type subscriberState[T any] struct {
	ch     chan Event[T]
	closed bool
}

// NewBroker creates a broker. buffer<=0 falls through to DefaultBuffer.
func NewBroker[T any](buffer int) *Broker[T] {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Broker[T]{
		subscribers: make(map[*subscriberState[T]]struct{}),
		buffer:      buffer,
	}
}

// Subscribe registers a new subscriber and returns a Subscription. The
// channel's buffer size is the broker's configured buffer.
func (b *Broker[T]) Subscribe() *Subscription[T] {
	s := &subscriberState[T]{ch: make(chan Event[T], b.buffer)}
	b.mu.Lock()
	b.subscribers[s] = struct{}{}
	b.mu.Unlock()

	return &Subscription[T]{
		ch: s.ch,
		cancel: func() {
			b.mu.Lock()
			if _, ok := b.subscribers[s]; ok {
				delete(b.subscribers, s)
				if !s.closed {
					s.closed = true
					close(s.ch)
				}
			}
			b.mu.Unlock()
		},
	}
}

// Publish fans e out to every subscriber. Never blocks. If a subscriber's
// channel is full, the oldest queued event is dropped and a Resync is
// enqueued in its place; e itself is discarded for that subscriber (the
// subscriber will re-fetch full state and observe e's effect there).
func (b *Broker[T]) Publish(e Event[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subscribers {
		if s.closed {
			continue
		}
		select {
		case s.ch <- e:
			// delivered
		default:
			// Channel full. Drop oldest, enqueue a Resync.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- Event[T]{Kind: KindResync}:
			default:
				// Even the Resync slot is contended; subscriber is wedged.
				// Skip — the next Publish will try again.
			}
		}
	}
}

// SubscriberCount reports the number of live subscribers. Useful for tests.
func (b *Broker[T]) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
