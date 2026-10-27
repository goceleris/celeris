package eventloop

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

type scriptedLoop struct {
	engine.WorkerLoop
	errs  []error // returned in order; the last repeats
	calls int
}

func (l *scriptedLoop) RegisterConn(int, func([]byte), func(error)) error {
	i := l.calls
	l.calls++
	if i >= len(l.errs) {
		i = len(l.errs) - 1
	}
	return l.errs[i]
}

var errBusy = errors.New("celeris/iouring: fd already registered")

func TestIsNumberBusy887(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errBusy, true},
		{errors.New("celeris/iouring: fd 7 is already an HTTP connection"), false},
		{errors.New("celeris/epoll: fd 7 is already registered"), false},
		{ErrAlreadyRegistered, false}, // a real duplicate of the standalone loop: it does not clear
		{fmt.Errorf("celeris/iouring: worker 0 has shut down, cannot register fd 7: %w", errors.New("engine shut down")), false},
	} {
		if got := IsNumberBusy(c.err); got != c.want {
			t.Errorf("IsNumberBusy(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRegisterConnWait887(t *testing.T) {
	other := errors.New("boom")
	for _, c := range []struct {
		name      string
		errs      []error
		cancelled bool
		wantErr   error
		wantCalls int
	}{
		{"free at once", []error{nil}, false, nil, 1},
		{"clears after three refusals", []error{errBusy, errBusy, errBusy, nil}, false, nil, 4},
		{"other error is final", []error{other}, false, other, 1},
		{"other error after a refusal is final", []error{errBusy, other}, false, other, 2},
		{"ctx done returns the refusal", []error{errBusy}, true, errBusy, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancelled {
				cancel()
			}
			l := &scriptedLoop{errs: c.errs}
			err := RegisterConnWait(ctx, l, 3, nil, nil)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if c.cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v does not wrap the ctx error: a caller cannot tell a cancelled wait from a refusal", err)
			}
			if l.calls != c.wantCalls {
				t.Fatalf("RegisterConn calls = %d, want %d", l.calls, c.wantCalls)
			}
		})
	}
}

// A refusal that never clears is bounded by registerWaitBudget, not by ctx.
func TestRegisterConnWaitIsBoundedWithoutACtxDeadline887(t *testing.T) {
	l := &scriptedLoop{errs: []error{errBusy}}
	start := time.Now()
	err := RegisterConnWait(context.Background(), l, 3, nil, nil)
	took := time.Since(start)
	if !errors.Is(err, errBusy) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, errRegisterWaitBudget) {
		t.Fatalf("err = %v does not say the wait ran out of budget", err)
	}
	if took < registerWaitBudget || took > registerWaitBudget+time.Second {
		t.Fatalf("gave up after %v, want about %v", took, registerWaitBudget)
	}
	t.Logf("gave up after %v and %d calls", took, l.calls)
}

// A ctx that is done before the wait stops it after the first refusal: no second
// RegisterConn call, which would register the fd for a caller that gave up.
// The wait's pause ends in a select on ctx.Done() and the pause's timer, and when
// the goroutine is not run for the pause (100 us) both are ready and Go picks at
// random: TestRegisterConnWait887/ctx_done_returns_the_refusal failed with
// "RegisterConn calls = 2, want 1" on a loaded CI runner (macOS portable
// fallback, celeris#1104). A first pause of 0 has the timer ready by the time
// the select runs, which is that interleaving without waiting for the scheduler.
func TestRegisterConnWaitCancelledCtxMakesNoSecondCall887(t *testing.T) {
	const waits = 2000
	extra := 0
	for range waits {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		l := &scriptedLoop{errs: []error{errBusy}}
		err := registerConnWait(ctx, l, 3, nil, nil, 0)
		if !errors.Is(err, errBusy) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the refusal wrapped with context.Canceled", err)
		}
		if l.calls != 1 {
			extra++
		}
	}
	if extra != 0 {
		t.Errorf("a ctx done before the wait was followed by a second RegisterConn call in %d of %d waits", extra, waits)
	}
}

// cancelAfterFirst887 is a scripted loop whose first call cancels ctx, as a ctx
// that ends while the wait is between the first refusal and its pause.
type cancelAfterFirst887 struct {
	scriptedLoop
	cancel context.CancelFunc
}

func (l *cancelAfterFirst887) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	err := l.scriptedLoop.RegisterConn(fd, onRecv, onClose)
	l.cancel()
	return err
}

// A ctx that is cancelled DURING the wait (by the first refusal, before the
// pause) stops it with no further RegisterConn call, even when the pause's timer
// is ready as well (a first pause of 0): the pause ends by whichever case the
// select picks and the ctx is looked at after it.
func TestRegisterConnWaitCancelledDuringTheWaitMakesNoFurtherCall887(t *testing.T) {
	const waits = 2000
	extra := 0
	for range waits {
		ctx, cancel := context.WithCancel(context.Background())
		l := &cancelAfterFirst887{scriptedLoop: scriptedLoop{errs: []error{errBusy}}, cancel: cancel}
		err := registerConnWait(ctx, l, 3, nil, nil, 0)
		cancel()
		if !errors.Is(err, errBusy) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the refusal wrapped with context.Canceled", err)
		}
		if l.calls != 1 {
			extra++
		}
	}
	if extra != 0 {
		t.Errorf("a ctx cancelled during the wait was followed by another RegisterConn call in %d of %d waits", extra, waits)
	}
}

// A ctx whose deadline has passed is the same: the refusal is wrapped with
// context.DeadlineExceeded and the first call is the only one.
func TestRegisterConnWaitDeadlinePassedMakesNoFurtherCall887(t *testing.T) {
	for range 200 {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		l := &scriptedLoop{errs: []error{errBusy}}
		err := registerConnWait(ctx, l, 3, nil, nil, 0)
		cancel()
		if !errors.Is(err, errBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the refusal wrapped with context.DeadlineExceeded", err)
		}
		if l.calls != 1 {
			t.Fatalf("RegisterConn calls = %d, want 1", l.calls)
		}
	}
}
