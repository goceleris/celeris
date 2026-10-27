//go:build unix

package postgres

// celeris#887 item 28 (see driver/redis/dial_refused_887_test.go): the dial
// must wait out a RegisterConn refusal that the io_uring engine gives for a
// number whose previous conn is not finalized yet.

import (
	"context"
	"errors"
	"net"
	"strings"
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

// hangUpListener accepts and closes every conn at once: a dial that gets past
// RegisterConn then fails in the startup (EOF), quickly, which is not the
// failure under test.
func hangUpListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln.Addr().String()
}

func TestDialWaitsOutAnUnfinalizedNumber887(t *testing.T) {
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	p := &refusingProvider{EventLoopProvider: prov, err: errors.New("celeris/iouring: fd already registered")}
	p.refusals.Store(3)

	host, port, _ := net.SplitHostPort(hangUpListener(t))
	dsn := DSN{
		Host: host, Port: port, User: "u", Database: "d",
		Options: Options{SSLMode: "disable", StatementCacheSize: 16},
		Params:  map[string]string{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialConn(ctx, p, nil, dsn, 0)
	if err == nil {
		_ = c.Close()
		t.Fatal("dial against a server that hangs up succeeded")
	}
	// Past the refusals the dial reaches the startup, which the server cannot
	// answer; the refusal itself must not be what the caller sees.
	if strings.Contains(err.Error(), "already registered") {
		t.Fatalf("dial returned the RegisterConn refusal: %v (RegisterConn was called %d times)", err, p.calls.Load())
	}
	if got := p.calls.Load(); got != 4 {
		t.Fatalf("RegisterConn calls = %d, want 4 (3 refusals, then the registration)", got)
	}
}
