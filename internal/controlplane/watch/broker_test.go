package watch_test

import (
	"sync"
	"testing"
	"time"

	"github.com/PatrickRuddiman/jaco/internal/controlplane/watch"
)

func TestBrokerDeliversAllEventsInOrder(t *testing.T) {
	b := watch.NewBroker[int](16)
	sub := b.Subscribe()
	t.Cleanup(sub.Cancel)

	for i := 1; i <= 10; i++ {
		b.Publish(watch.Event[int]{Kind: watch.KindUpdated, After: i, RaftIndex: uint64(i)})
	}

	for want := 1; want <= 10; want++ {
		select {
		case ev := <-sub.Events():
			if ev.Kind != watch.KindUpdated {
				t.Fatalf("event %d: kind=%v, want Updated", want, ev.Kind)
			}
			if ev.After != want {
				t.Fatalf("event %d: After=%d, want %d", want, ev.After, want)
			}
			if ev.RaftIndex != uint64(want) {
				t.Errorf("event %d: RaftIndex=%d, want %d", want, ev.RaftIndex, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", want)
		}
	}
}

func TestBrokerSlowSubscriberGetsResync(t *testing.T) {
	const buf = 16
	b := watch.NewBroker[int](buf)
	sub := b.Subscribe()
	t.Cleanup(sub.Cancel)

	// Publish far beyond the buffer without draining.
	for i := 0; i < buf+5; i++ {
		b.Publish(watch.Event[int]{Kind: watch.KindUpdated, After: i})
	}

	sawResync := false
	for drained := 0; drained < buf+5; drained++ {
		select {
		case ev := <-sub.Events():
			if ev.Kind == watch.KindResync {
				sawResync = true
			}
		case <-time.After(100 * time.Millisecond):
			// Channel empty; we're done draining.
			if !sawResync {
				t.Fatalf("expected at least one Resync event after overflow; drained=%d", drained)
			}
			return
		}
	}
	if !sawResync {
		t.Fatalf("expected at least one Resync event after overflow")
	}
}

func TestBrokerCancelIsIdempotent(t *testing.T) {
	b := watch.NewBroker[int](4)
	sub := b.Subscribe()

	if got := b.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount before cancel = %d, want 1", got)
	}
	sub.Cancel()
	sub.Cancel() // must not panic and must not double-close.
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount after cancel = %d, want 0", got)
	}
}

func TestBrokerPublishConcurrent(t *testing.T) {
	b := watch.NewBroker[int](1024)
	sub := b.Subscribe()
	t.Cleanup(sub.Cancel)

	const goroutines = 8
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				b.Publish(watch.Event[int]{Kind: watch.KindUpdated, After: id*1000 + i})
			}
		}(g)
	}

	// Drain in parallel so the channel never fills under the race detector.
	received := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for received < goroutines*perGoroutine {
			select {
			case <-sub.Events():
				received++
			case <-time.After(time.Second):
				return
			}
		}
	}()
	wg.Wait()
	<-done
	if received < goroutines*perGoroutine {
		t.Logf("received %d of %d (resync drops are acceptable; just guarding for total lockup)",
			received, goroutines*perGoroutine)
	}
}

func TestNewBrokerZeroBufferFallsBack(t *testing.T) {
	b := watch.NewBroker[int](0)
	sub := b.Subscribe()
	t.Cleanup(sub.Cancel)
	// Should accept many events without dropping if the default is used.
	for i := 0; i < watch.DefaultBuffer; i++ {
		b.Publish(watch.Event[int]{Kind: watch.KindAdded, After: i})
	}
	if got := len(sub.Events()); got != watch.DefaultBuffer {
		t.Fatalf("len(Events) after %d publishes = %d, want %d", watch.DefaultBuffer, got, watch.DefaultBuffer)
	}
}
