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
