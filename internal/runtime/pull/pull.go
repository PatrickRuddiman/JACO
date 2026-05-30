// Package pull implements docker image pulls with exponential backoff. The
// retry sequence is 1s, 2s, 4s, …, capped at 1h per retry, with no upper
// attempt count — the caller cancels the supplied context (typically when a
// fresh Deploy.Apply lands) to reset.
package pull

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/docker/docker/api/types/image"
)

// BackoffCap is the longest single sleep between attempts. Picked at 1h per
// the runtime slice (matches the cert-issuance retry shape).
const BackoffCap = time.Hour

// State is the string ReplicaObserved.state writes use during a pull.
type State string

const (
	StatePulling State = "pulling"
	StateFailed  State = "failed"
	StateDone    State = "done"
)

// StateFn receives every state transition during a pull. attempt counts up
// from 1; nextRetryAt is set only on failed-then-retrying transitions
// (zero time on pulling/done).
type StateFn func(state State, attempt int, nextRetryAt time.Time, lastErr error)

// Puller is the narrow docker subset Pull needs.
type Puller interface {
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
}

// Pull retries dockerx.ImagePull with exponential backoff until ctx is
// cancelled or the pull succeeds. after may be nil to use time.After;
// tests use a fake to avoid burning wall time. onState may be nil.
//
// auth is resolved once per attempt: a stable closure returning the same
// X-Registry-Auth blob lets the moby daemon reuse its scope-token cache
// across retries, while a closure that re-reads cluster state every call
// picks up a rotated credential without the reconciler having to cancel
// and restart the pull loop. auth may be nil — equivalent to anonymous.
func Pull(ctx context.Context, d Puller, ref string, auth AuthResolver, after func(time.Duration) <-chan time.Time, onState StateFn) error {
	if after == nil {
		after = time.After
	}
	if ref == "" {
		return errors.New("pull: ref is required")
	}

	for attempt := 1; ; attempt++ {
		notify(onState, StatePulling, attempt, time.Time{}, nil)
		opts := image.PullOptions{}
		if auth != nil {
			encoded, err := auth(ref)
			if err != nil {
				// A resolver error means the cluster state for this ref is
				// malformed (e.g. canonicalization failed) — surface it as a
				// pull failure so the operator sees it on ReplicaObserved
				// instead of looping silently.
				delay := BackoffDuration(attempt)
				next := time.Now().Add(delay)
				notify(onState, StateFailed, attempt, next, err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-after(delay):
					continue
				}
			}
			opts.RegistryAuth = encoded
		}
		rc, err := d.ImagePull(ctx, ref, opts)
		if err == nil {
			// Drain the pull stream so the layers finish downloading; the
			// docker API returns immediately once the request is accepted,
			// the actual transfer happens as the reader is consumed.
			_, drainErr := io.Copy(io.Discard, rc)
			_ = rc.Close()
			if drainErr == nil {
				notify(onState, StateDone, attempt, time.Time{}, nil)
				return nil
			}
			err = drainErr
		}

		// Pull failed (either at request time or during stream drain). Sleep
		// for the backoff window, then retry.
		delay := BackoffDuration(attempt)
		next := time.Now().Add(delay)
		notify(onState, StateFailed, attempt, next, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-after(delay):
			// continue
		}
	}
}

// BackoffDuration returns the sleep duration to apply after the attempt-th
// failure: 2^(attempt-1) seconds, capped at 1h.
func BackoffDuration(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Doubling shift: 1<<0 = 1, 1<<1 = 2, ..., 1<<11 = 2048, 1<<12 = 4096.
	// Guard against ridiculously large shifts.
	if attempt >= 32 {
		return BackoffCap
	}
	d := time.Second << uint(attempt-1)
	if d > BackoffCap || d < 0 {
		return BackoffCap
	}
	return d
}

func notify(fn StateFn, s State, attempt int, nextRetryAt time.Time, err error) {
	if fn == nil {
		return
	}
	fn(s, attempt, nextRetryAt, err)
}
