//go:build unix

package redis

// celeris#887 item 28, deterministic half: a WorkerLoop that refuses
// RegisterConn the way the io_uring engine does for a number whose previous
// conn is not finalized yet ("celeris/iouring: fd already registered", see
// engine.WorkerLoop.UnregisterConn). The real-engine half is
// TestDialAfterTheCloseOfAConnOnTheSameNumberSucceeds887.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// refusingProvider wraps the standalone loop. Its workers refuse RegisterConn
// with err while refusals is not 0 (a negative value refuses for ever) and
// count every call.
type refusingProvider struct {
	engine.EventLoopProvider
	refusals atomic.Int32
	calls    atomic.Int32
	err      error
}

func newRefusingProvider(t *testing.T, refusals int32, err error) *refusingProvider {
	t.Helper()
	prov, err2 := eventloop.Resolve(nil)
	if err2 != nil {
		t.Fatal(err2)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	p := &refusingProvider{EventLoopProvider: prov, err: err}
	p.refusals.Store(refusals)
	return p
}

func (p *refusingProvider) WorkerLoop(n int) engine.WorkerLoop {
	return refusingLoop{WorkerLoop: p.EventLoopProvider.WorkerLoop(n), p: p}
}

type refusingLoop struct {
	engine.WorkerLoop
	p *refusingProvider
}

func (l refusingLoop) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	l.p.calls.Add(1)
	if r := l.p.refusals.Load(); r != 0 {
		if r > 0 {
			l.p.refusals.Add(-1)
		}
		return l.p.err
	}
	return l.WorkerLoop.RegisterConn(fd, onRecv, onClose)
}

var errIOUringBusy887 = errors.New("celeris/iouring: fd already registered")

func dialRefusedCfg(t *testing.T) Config {
	t.Helper()
	fake := startFakeRedis(t, nil)
	return Config{Addr: fake.Addr(), ForceRESP2: true}
}

// A refusal that clears within a few milliseconds must not fail the dial.
func TestDialWaitsOutAnUnfinalizedNumber887(t *testing.T) {
	p := newRefusingProvider(t, 3, errIOUringBusy887)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialRedisConn(ctx, p, dialRefusedCfg(t), 0)
	if err != nil {
		t.Fatalf("dial: %v (RegisterConn was called %d times)", err, p.calls.Load())
	}
	_ = c.Close()
	if got := p.calls.Load(); got != 4 {
		t.Fatalf("RegisterConn calls = %d, want 4 (3 refusals, then the registration)", got)
	}
}

// A number that stays registered must not hold the dial for ever: the dial's
// ctx bounds the wait, and the refusal is what the caller sees.
func TestDialGivesUpWhenTheNumberStaysRegistered887(t *testing.T) {
	p := newRefusingProvider(t, -1, errIOUringBusy887)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, err := dialRedisConn(ctx, p, dialRefusedCfg(t), 0)
	took := time.Since(start)
	if err == nil {
		_ = c.Close()
		t.Fatal("dial succeeded against a loop that always refuses")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("dial error = %v, want the refusal", err)
	}
	if took > time.Second {
		t.Fatalf("dial took %v with a 300 ms ctx: the wait ignores the ctx", took)
	}
	t.Logf("gave up after %v and %d calls: %v", took, p.calls.Load(), err)
}

// Only the "number not finalized yet" refusal is retried. Any other
// RegisterConn error is final at once.
func TestDialDoesNotRetryOtherRegisterErrors887(t *testing.T) {
	other := errors.New("celeris/iouring: fd 7 is already an HTTP connection")
	p := newRefusingProvider(t, -1, other)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := dialRedisConn(ctx, p, dialRefusedCfg(t), 0)
	if !errors.Is(err, other) {
		t.Fatalf("dial error = %v, want %v", err, other)
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("RegisterConn calls = %d, want 1", got)
	}
}
