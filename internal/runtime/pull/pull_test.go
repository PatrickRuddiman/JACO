package pull_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"

	"github.com/PatrickRuddiman/jaco/internal/runtime/pull"
)

// fakeClock auto-advances by the requested delay on every after call, so
// Pull's backoff loop progresses immediately without burning wall time.
// delaysRequested records each requested delay in order so tests can assert
// the backoff sequence.
type fakeClock struct {
	mu              sync.Mutex
	delaysRequested []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.delaysRequested = append(c.delaysRequested, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func (c *fakeClock) Delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.delaysRequested))
	copy(out, c.delaysRequested)
	return out
}

// fakePuller serves up canned responses to ImagePull keyed by call index.
type fakePuller struct {
	calls    int64
	responses []func() (io.ReadCloser, error)
}

func (f *fakePuller) ImagePull(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
	idx := atomic.AddInt64(&f.calls, 1) - 1
	if int(idx) >= len(f.responses) {
		return nil, fmt.Errorf("transient (call %d)", idx+1)
	}
	return f.responses[idx]()
}

func okStream() (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(`{"status":"Status: Image is up to date"}`)), nil
}

func failOnce() (io.ReadCloser, error) {
	return nil, errors.New("transient network error")
}

func TestBackoffDuration_Sequence(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{11, 1024 * time.Second},
		{12, 2048 * time.Second},
		{13, 3600 * time.Second}, // first capped attempt: 2^12=4096 > 3600
		{20, 3600 * time.Second},
		{100, 3600 * time.Second},
	}
	for _, c := range cases {
		got := pull.BackoffDuration(c.attempt)
		if got != c.want {
			t.Errorf("BackoffDuration(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestPull_SucceedsAfterTransientFailures(t *testing.T) {
	d := &fakePuller{responses: []func() (io.ReadCloser, error){
		failOnce, failOnce, failOnce, okStream,
	}}
	clock := newFakeClock()

	err := pull.Pull(context.Background(), d, "nginx:1.27", clock.After, nil)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := atomic.LoadInt64(&d.calls); got != 4 {
		t.Errorf("ImagePull calls = %d, want 4", got)
	}
	// Three retries → three backoff sleeps: 1s, 2s, 4s.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	delays := clock.Delays()
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i, d := range delays {
		if d != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, d, want[i])
		}
	}
}

func TestPull_BackoffSequenceCapsAt3600AtAttempt13(t *testing.T) {
	// Run Pull through 13 failed attempts; the 13th sleep should be 3600s.
	const target = 13
	d := &fakePuller{}
	// Always fail; we cancel after the 13th sleep is recorded.
	clock := newFakeClock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Watch the delay log; once we see 13 entries, cancel.
	go func() {
		for {
			if len(clock.Delays()) >= target {
				cancel()
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	err := pull.Pull(ctx, d, "img", clock.After, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	delays := clock.Delays()
	if len(delays) < target {
		t.Fatalf("got %d delays, want at least %d", len(delays), target)
	}
	// Verify the 13th delay is the cap.
	if delays[12] != 3600*time.Second {
		t.Errorf("delays[12] = %v, want 3600s", delays[12])
	}
	// Earlier delays follow the doubling sequence.
	for i := 0; i < 12; i++ {
		want := time.Second << uint(i)
		if delays[i] != want {
			t.Errorf("delays[%d] = %v, want %v", i, delays[i], want)
		}
	}
}

func TestPull_CancellationReturnsContextErr(t *testing.T) {
	d := &fakePuller{} // always fails
	// Non-advancing after — Pull blocks until we cancel.
	blocked := make(chan time.Time)
	after := func(time.Duration) <-chan time.Time { return blocked }

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := pull.Pull(ctx, d, "img", after, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPull_EmptyRefRejected(t *testing.T) {
	err := pull.Pull(context.Background(), &fakePuller{}, "", newFakeClock().After, nil)
	if err == nil {
		t.Fatalf("expected error on empty ref")
	}
}

func TestPull_StateCallbackTransitions(t *testing.T) {
	d := &fakePuller{responses: []func() (io.ReadCloser, error){
		failOnce, failOnce, okStream,
	}}
	clock := newFakeClock()

	var states []pull.State
	var attempts []int
	cb := func(s pull.State, attempt int, _ time.Time, _ error) {
		states = append(states, s)
		attempts = append(attempts, attempt)
	}

	if err := pull.Pull(context.Background(), d, "img", clock.After, cb); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// Expected sequence: pulling(1), failed(1), pulling(2), failed(2), pulling(3), done(3).
	wantStates := []pull.State{
		pull.StatePulling, pull.StateFailed,
		pull.StatePulling, pull.StateFailed,
		pull.StatePulling, pull.StateDone,
	}
	if len(states) != len(wantStates) {
		t.Fatalf("states len = %d, want %d (got %v)", len(states), len(wantStates), states)
	}
	for i, s := range wantStates {
		if states[i] != s {
			t.Errorf("states[%d] = %v, want %v", i, states[i], s)
		}
	}
	wantAttempts := []int{1, 1, 2, 2, 3, 3}
	for i, a := range wantAttempts {
		if attempts[i] != a {
			t.Errorf("attempts[%d] = %d, want %d", i, attempts[i], a)
		}
	}
}
