//go:build unix

package memcached

// celeris#887 item 28 (see driver/redis/dial_refused_887_test.go): the dial
// must wait out a RegisterConn refusal that the io_uring engine gives for a
// number whose previous conn is not finalized yet.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

type refusingProvider struct {
	engine.EventLoopProvider
	refusals atomic.Int32
	calls    atomic.Int32
	err      error
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

func TestDialWaitsOutAnUnfinalizedNumber887(t *testing.T) {
	p := newRefusingProvider(t, 3, errors.New("celeris/iouring: fd already registered"))
	fake := startFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialMemcachedConn(ctx, p, Config{Addr: fake.Addr()}, 0)
	if err != nil {
		t.Fatalf("dial: %v (RegisterConn was called %d times)", err, p.calls.Load())
	}
	_ = c.Close()
	if got := p.calls.Load(); got != 4 {
		t.Fatalf("RegisterConn calls = %d, want 4 (3 refusals, then the registration)", got)
	}
}

func TestDialDoesNotRetryOtherRegisterErrors887(t *testing.T) {
	other := errors.New("celeris/iouring: fd 7 is already an HTTP connection")
	p := newRefusingProvider(t, -1, other)
	fake := startFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := dialMemcachedConn(ctx, p, Config{Addr: fake.Addr()}, 0)
	if !errors.Is(err, other) {
		t.Fatalf("dial error = %v, want %v", err, other)
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("RegisterConn calls = %d, want 1", got)
	}
}
