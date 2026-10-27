package eventloop

import (
	"context"
	"errors"
	"fmt"
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

// errRegisterWaitBudget is wrapped into the error RegisterConnWait returns
// when registerWaitBudget has passed (declared by the tests commit, used by the
// next).
var errRegisterWaitBudget = errors.New("celeris/eventloop: waited for the number to be finalized, budget used up")

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
// registerWaitBudget has passed, and then returns the refusal wrapped together
// with the reason it stopped (ctx.Err(), or errRegisterWaitBudget), so
// errors.Is sees both. Every other error is returned at once, and a failed
// call leaves fd with the caller, as RegisterConn does. The drivers' dials use
// it: without it Pool.Acquire fails under connection churn on the io_uring
// engine (celeris#887 item 28).
//
// It must not run on the worker goroutine of loop (a handler of the server
// whose engine is that loop's, called inline): the number is finalized by that
// worker, which would be asleep in this wait, so the wait runs to its end,
// stalls every connection of that worker for up to registerWaitBudget, and
// returns the refusal anyway. The drivers do not dial there: with WithEngine on
// io_uring, epoll or adaptive their command pools dial direct net.Conns and
// never call RegisterConn. The one public path left is redis
// Client.Subscribe called from a handler, whose PubSub pool still registers
// with the engine; with the default config its handshake already blocks on the
// worker it occupies, so the case that reaches this wait is ForceRESP2 with no
// password and no DB. The engine gives no cheap, reliable way to tell that the
// caller is on its worker, so this is documented, not guarded. The alternative
// that works on any thread, moving the socket to a free number (F_DUPFD) before
// registering it, changes which descriptor the driver owns and needs its own
// review.
func RegisterConnWait(ctx context.Context, loop engine.WorkerLoop, fd int, onRecv func([]byte), onClose func(error)) error {
	return registerConnWait(ctx, loop, fd, onRecv, onClose, registerFirstPause)
}

// registerFirstPause is the first pause of RegisterConnWait; it doubles up to 5 ms.
const registerFirstPause = 100 * time.Microsecond

// registerConnWait is RegisterConnWait with the first pause given, so a test can
// make the pause's timer ready as soon as the wait starts.
func registerConnWait(ctx context.Context, loop engine.WorkerLoop, fd int, onRecv func([]byte), onClose func(error), firstPause time.Duration) error {
	err := loop.RegisterConn(fd, onRecv, onClose)
	if !IsNumberBusy(err) {
		return err
	}
	deadline := time.Now().Add(registerWaitBudget)
	pause := firstPause
	for {
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (gave up waiting: %w)", err, ctx.Err())
		case <-t.C:
		}
		if err = loop.RegisterConn(fd, onRecv, onClose); !IsNumberBusy(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w (gave up waiting: %w)", err, errRegisterWaitBudget)
		}
		if pause < 5*time.Millisecond {
			pause *= 2
		}
	}
}
