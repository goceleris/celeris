package timeout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// TestLazyDeadlineKeepsEarlierParentDeadline1393: like context.WithDeadline,
// the lazy context reports the earlier of its own deadline and its parent's.
// The margins are an hour, so the result does not depend on timing.
func TestLazyDeadlineKeepsEarlierParentDeadline1393(t *testing.T) {
	now := time.Now()
	own := now.Add(2 * time.Hour)
	earlier := now.Add(time.Hour)
	later := now.Add(3 * time.Hour)

	cases := []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
		want   time.Time
	}{
		{"parent earlier than own", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), earlier)
		}, earlier},
		{"parent later than own", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), later)
		}, own},
		{"parent without a deadline", func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, own},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := tc.parent()
			defer cancel()
			lazy := newLazyDeadlineCtx(parent, own)
			got, ok := lazy.Deadline()
			if !ok {
				t.Fatal("Deadline() reported no deadline")
			}
			if !got.Equal(tc.want) {
				t.Fatalf("Deadline() = %v, want %v", got, tc.want)
			}
			if lazy.expired() {
				t.Fatal("expired() = true an hour before the deadline")
			}
		})
	}
}

// TestLazyDeadlineExpiredParent1393: a parent whose deadline has already
// passed makes the lazy context expired, without anything reading Done().
func TestLazyDeadlineExpiredParent1393(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	lazy := newLazyDeadlineCtx(parent, time.Now().Add(time.Hour))
	if !lazy.expired() {
		t.Fatal("expired() = false for a parent whose deadline passed an hour ago")
	}
	if err := lazy.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded", err)
	}
}

// TestNestedTimeoutReportsOuterDeadline1393: a timeout middleware inside an
// outer one with a shorter timeout reports the outer deadline from
// Context().Deadline().
func TestNestedTimeoutReportsOuterDeadline1393(t *testing.T) {
	var outer, inner time.Time
	var outerOK, innerOK bool
	chain := []celeris.HandlerFunc{
		New(Config{Timeout: time.Hour}),
		func(c *celeris.Context) error {
			outer, outerOK = c.Context().Deadline()
			return c.Next()
		},
		New(Config{Timeout: 2 * time.Hour}),
		func(c *celeris.Context) error {
			inner, innerOK = c.Context().Deadline()
			return c.String(200, "ok")
		},
	}
	rec, err := testutil.RunChain(t, chain, "GET", "/nested")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !outerOK || !innerOK {
		t.Fatalf("deadlines reported: outer %v, inner %v; want both", outerOK, innerOK)
	}
	if !inner.Equal(outer) {
		t.Fatalf("inner Deadline() = %v, want the outer %v", inner, outer)
	}
}

// TestTimeoutExpiredParentDeadline1393: a request whose context deadline
// passed before the timeout middleware ran is answered as a timeout, as it
// would be under context.WithTimeout, even though the handler itself
// returned without error.
func TestTimeoutExpiredParentDeadline1393(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	chain := []celeris.HandlerFunc{
		func(c *celeris.Context) error {
			c.SetContext(parent)
			return c.Next()
		},
		New(Config{Timeout: time.Hour}),
		func(_ *celeris.Context) error { return nil },
	}
	_, err := testutil.RunChain(t, chain, "GET", "/expired")
	testutil.AssertHTTPError(t, err, 503)
}
