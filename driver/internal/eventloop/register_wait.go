package eventloop

import (
	"context"
	"strings"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// registerWaitBudget bounds how long RegisterConnWait waits for a refused
// number to be finalized. A conn is finalized when the engine has cancelled its
// operations and closed its own duplicate of the descriptor, normally within
// microseconds of UnregisterConn; a close that lingers (SO_LINGER) is closed
// off the worker and can take longer (celeris#735), so the wait is bounded
// instead of unlimited.
const registerWaitBudget = 2 * time.Second

// IsNumberBusy reports whether err is the io_uring engine's refusal of a
// descriptor number whose previous conn has been unregistered but not yet
// finalized: "celeris/iouring: fd already registered".
// [engine.WorkerLoop.UnregisterConn] documents it: the next socket usually
// gets the lowest free number, which is the number of the conn just closed,
// and the engine keeps that number registered until the old conn is finalized.
// It clears by itself, unlike any other RegisterConn error (the number is
// held by an HTTP conn, the worker has shut down, a duplicate registration on
// the epoll engine, whose onClose has run before UnregisterConn returns).
//
// The engine has no sentinel for it, so the text is matched; the tests of
// the drivers register the same number twice on a real io_uring worker to pin
// it.
func IsNumberBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "celeris/iouring:") && strings.HasSuffix(msg, "fd already registered")
}

// RegisterConnWait is loop.RegisterConn that waits out the io_uring engine's
// "fd already registered" refusal (see [IsNumberBusy]) instead of returning it:
// it retries with a growing pause until the number is free, ctx is done, or
// registerWaitBudget has passed, and then returns the refusal. Every other
// error is returned at once, and a failed call leaves fd with the caller, as
// RegisterConn does. The drivers' dials use it: without it Pool.Acquire fails
// under connection churn on the io_uring engine (celeris#887 item 28).
func RegisterConnWait(ctx context.Context, loop engine.WorkerLoop, fd int, onRecv func([]byte), onClose func(error)) error {
	err := loop.RegisterConn(fd, onRecv, onClose)
	if !IsNumberBusy(err) {
		return err
	}
	deadline := time.Now().Add(registerWaitBudget)
	pause := 100 * time.Microsecond
	for {
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		if err = loop.RegisterConn(fd, onRecv, onClose); !IsNumberBusy(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return err
		}
		if pause < 5*time.Millisecond {
			pause *= 2
		}
	}
}
