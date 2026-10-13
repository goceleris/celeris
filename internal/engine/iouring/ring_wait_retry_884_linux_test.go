//go:build linux

package iouring

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// celeris#884 item 49, the helper's own contract, called directly on a worker
// built around a ring (the engine arms of run_ebusy_884_linux_test.go inject
// after a wait that succeeded, so they reach neither the sleep branch nor the
// warning with the state the helper decides on).

// warnLog884 records the records a worker logs.
type warnLog884 struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *warnLog884) Enabled(context.Context, slog.Level) bool { return true }
func (l *warnLog884) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *warnLog884) WithGroup(string) slog.Handler            { return l }
func (l *warnLog884) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	return nil
}

func (l *warnLog884) warns() (n int, last slog.Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.Level == slog.LevelWarn {
			n++
			last = r
		}
	}
	return n, last
}

// postNOP884 puts one completion in the CQ ring, unreaped.
func postNOP884(t *testing.T, r *Ring) {
	t.Helper()
	sqe := r.GetSQE()
	if sqe == nil {
		t.Fatal("SQ ring full")
	}
	*(*uint8)(sqe) = opNOP
	*(*uint64)(unsafe.Add(sqe, 32)) = 0
	if _, err := r.Submit(); err != nil {
		t.Fatalf("submit NOP: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h, tl := r.BeginCQ(); h != tl {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the NOP's completion never reached the CQ ring")
}

// TestRingWaitContinuesBacksOffAndWarnsOnce884: a retryable error with
// nothing in the CQ ring to reap makes the worker sleep (at least 50 us)
// before it enters again, and one with a completion waiting does not; the
// first retried wait is logged once as a Warn, whatever follows, so an
// operator on a 5.19 to 6.1 kernel can see the loop retrying (there is no
// other signal: the counter is not a public metric).
func TestRingWaitContinuesBacksOffAndWarnsOnce884(t *testing.T) {
	ring := newTestRing(t)
	logs := &warnLog884{}
	w := &Worker{ring: ring, id: 7, logger: slog.New(logs)}

	for i := 1; i <= 3; i++ {
		start := time.Now()
		if !w.ringWaitContinues(waitModeTimed, ebusyErr884()) {
			t.Fatalf("pass %d: an EBUSY wait on an empty CQ ring ended the worker", i)
		}
		if took := time.Since(start); took < 50*time.Microsecond {
			t.Errorf("pass %d: an EBUSY wait with nothing to reap returned after %v, want a sleep of at least 50us", i, took)
		}
		if got := w.ringWaitRetries.Load(); got != uint64(i) {
			t.Errorf("pass %d: ringWaitRetries=%d, want %d", i, got, i)
		}
	}
	if n, last := logs.warns(); n != 1 {
		t.Errorf("3 retried waits logged %d warnings, want exactly 1 (the first)", n)
	} else {
		var worker int64 = -1
		last.Attrs(func(a slog.Attr) bool {
			if a.Key == "worker" {
				worker = a.Value.Int64()
			}
			return true
		})
		if worker != 7 {
			t.Errorf("the warning names worker %d, want 7: %q", worker, last.Message)
		}
	}

	// A completion waiting: the loop's own pass reaps it, so no sleep. The
	// fastest of many calls is compared with the sleep's floor, as one slow
	// call (a descheduled test thread) says nothing.
	postNOP884(t, ring)
	fastest := time.Hour
	for range 20 {
		start := time.Now()
		if !w.ringWaitContinues(waitModeSubmitSends, ebusyErr884()) {
			t.Fatal("an EBUSY wait on a CQ ring with a completion in it ended the worker")
		}
		fastest = min(fastest, time.Since(start))
	}
	if fastest >= 50*time.Microsecond {
		t.Errorf("an EBUSY wait with a completion to reap still took at least %v: it sleeps when it should reap", fastest)
	}
	if n, _ := logs.warns(); n != 1 {
		t.Errorf("23 retried waits logged %d warnings, want exactly 1", n)
	}

	// Other errors are not retried, not counted and not warned about.
	before := w.ringWaitRetries.Load()
	if w.ringWaitContinues(waitModeTimed, unix.EIO) {
		t.Error("an EIO wait did not end the worker")
	}
	if got := w.ringWaitRetries.Load(); got != before {
		t.Errorf("an EIO wait was counted as a retry (%d -> %d)", before, got)
	}
}

// TestRetryPendingKeepsTheSQEsOfARefusedEnter884: the loop goes on after an
// EBUSY wait only because the SQEs the enter did not take are offered again
// by the next enter. The kernel answers EBUSY and EAGAIN before it consumed
// any SQE (the 5.19 to 6.1 cq-overflow case returns it with nothing
// submitted), so all of them stay pending; every other errno is a consumed
// batch, as a double submit of an SQE the kernel owns would be worse.
func TestRetryPendingKeepsTheSQEsOfARefusedEnter884(t *testing.T) {
	for _, tc := range []struct {
		errno unix.Errno
		want  uint32
	}{
		{unix.EBUSY, 5}, {unix.EAGAIN, 5},
		{unix.EINTR, 0}, {unix.ETIME, 0}, {unix.EIO, 0},
	} {
		if got := retryPending(5, tc.errno); got != tc.want {
			t.Errorf("retryPending(5, %v)=%d, want %d", tc.errno, got, tc.want)
		}
	}
}
