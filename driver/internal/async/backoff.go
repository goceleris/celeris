package async

import (
	"math/rand/v2"
	"time"
)

// Backoff computes exponentially-increasing delays for retry loops with
// optional multiplicative jitter.
//
// The zero value is unusable: callers must set Base and Cap explicitly (or
// rely on NewBackoff). Jitter of zero disables jitter.
//
// Backoff is not safe for concurrent use; it is intended to be owned by one
// retry loop at a time.
type Backoff struct {
	// Base is the initial delay. Defaults to 50ms when <= 0.
	Base time.Duration
	// Cap is the maximum delay. Defaults to 5s when <= 0.
	Cap time.Duration
	// Jitter is the jitter factor in [0, 1]. A value of 0 disables jitter.
	// Values above 1 are clamped to 1; negative values to 0.
	Jitter float64

	// attempts tracks successive Next calls for the Reset helper.
	attempts int
}

// NewBackoff returns a Backoff with a 0.2 jitter factor and the provided
// base/cap (or the package defaults if either is <= 0).
func NewBackoff(base, cap time.Duration) *Backoff {
	return &Backoff{Base: base, Cap: cap, Jitter: 0.2}
}

// Next returns the delay for attempt n (0-indexed) and advances the internal
// attempt counter. The returned delay is in
// [d*(1-Jitter), d*(1+Jitter)] where d = min(Base * 2^n, Cap).
func (b *Backoff) Next(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	cap := b.Cap
	if cap <= 0 {
		cap = 5 * time.Second
	}
	jitter := b.Jitter
	if jitter < 0 {
		jitter = 0
	} else if jitter > 1 {
		jitter = 1
	}
	if attempt < 0 {
		attempt = 0
	}

	// Compute base * 2^attempt with overflow protection.
	var d time.Duration
	// Cap at 62 shifts so int64 doesn't overflow; well beyond any realistic
	// Cap.
	if attempt >= 62 {
		d = cap
	} else {
		shifted := base << uint(attempt)
		if shifted <= 0 || shifted > cap {
			d = cap
		} else {
			d = shifted
		}
	}

	if jitter > 0 {
		// Multiplicative jitter in [1-jitter, 1+jitter].
		delta := (rand.Float64()*2 - 1) * jitter
		scaled := float64(d) * (1 + delta)
		if scaled < 0 {
			scaled = 0
		}
		d = time.Duration(scaled)
	}

	b.attempts = attempt + 1
	return d
}

// Reset clears the internal attempt counter. The next call to Next(0) will
// return a delay near Base.
func (b *Backoff) Reset() {
	b.attempts = 0
}

// Attempts returns the number of Next calls observed since the last Reset.
func (b *Backoff) Attempts() int {
	return b.attempts
}
