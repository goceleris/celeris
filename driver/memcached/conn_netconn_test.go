package memcached

// The net.Conn transport (conn_netconn.go), which is the only transport on a
// platform without unix file descriptors (windows). It is compiled on every
// platform, so these tests run on Linux CI and on windows-latest alike, over
// a portable eventloop (eventloop.NewPortable) and an in-process fake server
// on loopback TCP.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

func netConnProvider(t *testing.T) *eventloop.Loop {
	t.Helper()
	l, err := eventloop.NewPortable(2)
	if err != nil {
		t.Fatalf("NewPortable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

type zeroWorkerProvider struct{}

func (zeroWorkerProvider) NumWorkers() int                  { return 0 }
func (zeroWorkerProvider) WorkerLoop(int) engine.WorkerLoop { panic("no workers") }

type noNetConnProvider struct{}

type noNetConnWorker struct{ engine.WorkerLoop }

func (noNetConnProvider) NumWorkers() int                  { return 1 }
func (noNetConnProvider) WorkerLoop(int) engine.WorkerLoop { return noNetConnWorker{} }

// waitServerSawEOF fails unless the fake's first accepted conn is closed from
// the client side.
func waitServerSawEOF(t *testing.T, f *fakeMemcached) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.conns)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake server never accepted the conn")
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	srv := f.conns[0]
	f.mu.Unlock()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := srv.Read(buf); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatal("the server never saw the client's socket close")
			}
			return
		}
	}
}

func TestMemcachedNetConnRoundTripAndClose(t *testing.T) {
	for _, proto := range []struct {
		name  string
		p     Protocol
		start func(*testing.T) (addr string, fake *fakeMemcached)
	}{
		{"text", ProtocolText, func(t *testing.T) (string, *fakeMemcached) {
			f := startFake(t)
			return f.Addr(), f
		}},
		// The binary fake does not record its conns: the round trip and the
		// loop's side of Close are checked, the server's view is not.
		{"binary", ProtocolBinary, func(t *testing.T) (string, *fakeMemcached) {
			return startFakeBinary(t).Addr(), nil
		}},
	} {
		t.Run(proto.name, func(t *testing.T) {
			addr, fake := proto.start(t)
			prov := netConnProvider(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			c, err := dialMemcachedConnNetConn(ctx, prov, Config{Addr: addr, Protocol: proto.p}, 1)
			if err != nil {
				t.Fatalf("dialMemcachedConnNetConn: %v", err)
			}
			if !eventloop.IsNetConnID(c.fd) {
				t.Fatalf("c.fd = %d, want a net.Conn identifier", c.fd)
			}
			if c.file != nil || c.useDirect {
				t.Fatalf("file=%v useDirect=%v: the conn must hold neither a descriptor nor a direct net.Conn", c.file, c.useDirect)
			}
			if c.workerID != 1 {
				t.Fatalf("workerID = %d, want the hint 1", c.workerID)
			}
			for i := 0; i < 3; i++ {
				if err := c.Ping(ctx); err != nil {
					t.Fatalf("Ping #%d: %v", i, err)
				}
			}
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := c.loop.UnregisterConn(c.fd); !errors.Is(err, engine.ErrUnknownFD) {
				t.Fatalf("UnregisterConn after Close: got %v, want ErrUnknownFD", err)
			}
			if fake != nil {
				waitServerSawEOF(t, fake)
			}
			if err := c.Ping(ctx); !errors.Is(err, ErrClosed) {
				t.Fatalf("Ping after Close: got %v, want ErrClosed", err)
			}
		})
	}
}

func TestMemcachedNetConnServerHangUpFailsLaterCommands(t *testing.T) {
	fake := startFake(t)
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialMemcachedConnNetConn(ctx, prov, Config{Addr: fake.Addr()}, 0)
	if err != nil {
		t.Fatalf("dialMemcachedConnNetConn: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	fake.mu.Lock()
	for _, sc := range fake.conns {
		_ = sc.Close()
	}
	fake.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for !c.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the conn never noticed the server hang up")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping on a conn whose server hung up succeeded")
	}
}

func TestMemcachedNetConnDialErrorsCloseTheSocket(t *testing.T) {
	closed := netConnProvider(t)
	_ = closed.Close() // a closed loop refuses RegisterNetConn with ErrLoopClosed
	for _, tc := range []struct {
		name string
		prov engine.EventLoopProvider
		want string
		is   error
	}{
		{"zero workers", zeroWorkerProvider{}, "0 workers", nil},
		{"worker cannot adopt a net.Conn", noNetConnProvider{}, "cannot adopt a net.Conn", nil},
		{"loop closed", closed, "", eventloop.ErrLoopClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFake(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := dialMemcachedConnNetConn(ctx, tc.prov, Config{Addr: fake.Addr()}, 0)
			switch {
			case err == nil:
				t.Fatal("dial succeeded")
			case tc.is != nil && !errors.Is(err, tc.is):
				t.Fatalf("dial error = %v, want %v", err, tc.is)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("dial error = %v, want one containing %q", err, tc.want)
			}
			waitServerSawEOF(t, fake)
		})
	}
}
