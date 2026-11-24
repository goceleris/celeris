package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The end of a command's context, on a direct-mode conn.
//
// A direct conn waits for the reply in a socket read on the caller's goroutine
// (execDirectBuf, execManyDirect), which looks at no context. Two things end it:
//
//   - A deadline. The socket's deadline is set from ctx.Deadline() for the command
//     and cleared after it (as in v1.5.11), nothing is registered, and the read
//     that times out is reported as the context's error (ctxIOError), not as the
//     socket's raw "i/o timeout".
//   - The cancel() of a context that has no deadline, for a command that blocks on
//     the server (BLPOP, BRPOP, BLMOVE, BZPOPMIN, XREAD with BLOCK, WAIT, ... see
//     isBlockingCommand): it waits for as long as the command says (BLPOP key 0:
//     for ever), so a cancel is the only thing that can end it. (A context with a
//     deadline ends it at the deadline, by the socket.) The design of the memcached driver
//     (celeris#1213, #1218): context.AfterFunc sets the socket's deadline to "now"
//     when the context ends, which ends the blocked read. It is armed for these
//     commands only, so that a command under a context that cannot end, an ordinary
//     command or a pipeline under a cancel-only context costs nothing: they are
//     what v1.5.11 was, and wait for the server's answer.
//
// What a command that the context ended leaves behind: the reply of the command
// is still on its way, so the conn is closed (closeWithErr, as for every read or
// write error of a direct conn) and the pool discards it; no later command can
// receive that reply.

// isBlockingCommand reports whether args is a command that blocks on the server
// until it has something to answer, however long that takes: the blocking list
// and sorted-set pops, BLMOVE and BRPOPLPUSH, XREAD and XREADGROUP with BLOCK, WAIT
// and WAITAOF. args[0] is the command name in any case.
func isBlockingCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	v := args[0]
	switch len(v) {
	case 4:
		return strings.EqualFold(v, "WAIT")
	case 5:
		return strings.EqualFold(v, "BLPOP") || strings.EqualFold(v, "BRPOP") ||
			(strings.EqualFold(v, "XREAD") && hasBlockOption(args))
	case 6:
		return strings.EqualFold(v, "BLMOVE") || strings.EqualFold(v, "BLMPOP") || strings.EqualFold(v, "BZMPOP")
	case 7:
		return strings.EqualFold(v, "WAITAOF")
	case 8:
		return strings.EqualFold(v, "BZPOPMIN") || strings.EqualFold(v, "BZPOPMAX")
	case 10:
		return strings.EqualFold(v, "BRPOPLPUSH") || (strings.EqualFold(v, "XREADGROUP") && hasBlockOption(args))
	}
	return false
}

// hasBlockOption reports whether args of XREAD or XREADGROUP carry BLOCK before
// the STREAMS keyword (after it, BLOCK would be a key or an id).
func hasBlockOption(args []string) bool {
	for _, a := range args[1:] {
		if len(a) == 5 && strings.EqualFold(a, "BLOCK") {
			return true
		}
		if len(a) == 7 && strings.EqualFold(a, "STREAMS") {
			return false
		}
	}
	return false
}

// testHookWakeDelay, when set, makes the wake callback of armWake sleep that long
// before it acts, so that a test can hold a callback in flight across the end of
// its command. It is nil in production.
var testHookWakeDelay atomic.Pointer[time.Duration]

// wakeState is the wake of a direct conn's blocked read on the end of the caller's
// context (armWake). It lives on the conn and its callback is built once per conn,
// so that a command allocates none of it: what a command with a context that can
// end pays is context.AfterFunc's own two allocations.
type wakeState struct {
	mu    sync.Mutex
	fired bool
	done  bool
	fn    func()
	stop  func() bool
	// ran has capacity 1: the callback signals it when it has run, and disarmWake
	// waits on it whenever stopping the wake failed, so that a callback that has
	// started when the command ends cannot run during the next command on the conn.
	ran chan struct{}
	tcp *net.TCPConn
}

// armWake arranges that, when ctx ends, the socket of the direct conn c gets a
// deadline of "now", which wakes a read blocked on a silent server. The caller
// holds c.writerMu and pairs the call with disarmWake.
func (c *redisConn) armWake(ctx context.Context) {
	w := &c.wake
	if w.fn == nil {
		tcp := c.tcp // Close sets c.tcp to nil
		w.tcp = tcp
		w.ran = make(chan struct{}, 1)
		w.fn = func() {
			if d := testHookWakeDelay.Load(); d != nil {
				time.Sleep(*d)
			}
			w.mu.Lock()
			if !w.done {
				w.fired = true
				_ = tcp.SetDeadline(time.Now())
			}
			w.mu.Unlock()
			select {
			case w.ran <- struct{}{}:
			default:
			}
		}
	}
	w.mu.Lock()
	w.fired, w.done = false, false
	w.mu.Unlock()
	w.stop = context.AfterFunc(ctx, w.fn)
}

// disarmWake ends the wake armed by armWake; it does nothing if none is armed. It
// also undoes a wake that raced with a command that had just succeeded, so the
// conn is not left with a deadline in the past, and it waits for a callback that
// has started, so that one cannot reach the next command on the conn.
func (c *redisConn) disarmWake() {
	w := &c.wake
	if w.stop == nil {
		return
	}
	if !w.stop() {
		// The context has ended and the callback has been started on its own
		// goroutine (stop is called once per arm): it signals ran when done.
		<-w.ran
	}
	w.stop = nil
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.fired && !c.closed.Load() {
		_ = w.tcp.SetDeadline(time.Time{})
	}
}

// ctxIOError is the error a direct conn's read or write returned, as the caller
// should see it. A socket deadline that ended it because the context's deadline was
// reached, or because the context ended (the watcher of a blocking command on its
// cancel), is reported as the context's error, wrapped around the socket's so that
// both match errors.Is (context.DeadlineExceeded or context.Canceled, and
// os.ErrDeadlineExceeded). Whether the deadline was reached is decided by the clock
// against ctx.Deadline(): the socket's deadline can fire a hair before the context's
// own timer reports the expiry. Any other error is returned as it is.
func ctxIOError(ctx context.Context, err error) error {
	if err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}
