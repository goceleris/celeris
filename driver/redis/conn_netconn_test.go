package redis

// The net.Conn transport (conn_netconn.go), which is the only transport on a
// platform without unix file descriptors (windows). It is compiled on every
// platform, so these tests run on Linux CI and on windows-latest alike, over
// a portable eventloop (eventloop.NewPortable) and an in-process fake server
// on loopback TCP.

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

func netConnTestServer(t *testing.T) *fakeRedis {
	t.Helper()
	return startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			writeBulk(w, "value-of-"+cmd[1])
		case "PING":
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
	})
}

func netConnProvider(t *testing.T) *eventloop.Loop {
	t.Helper()
	l, err := eventloop.NewPortable(2)
	if err != nil {
		t.Fatalf("NewPortable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestRedisNetConnRoundTripAndClose(t *testing.T) {
	fake := netConnTestServer(t)
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := dialRedisConnNetConn(ctx, prov, Config{Addr: fake.Addr()}, 1)
	if err != nil {
		t.Fatalf("dialRedisConnNetConn: %v", err)
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
		req, err := c.exec(ctx, "GET", "k")
		if err != nil {
			t.Fatalf("GET #%d: %v", i, err)
		}
		if got := string(req.result.Str); got != "value-of-k" {
			t.Fatalf("GET #%d = %q", i, got)
		}
		c.releaseResult(req)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close handed the conn back to the loop, which closed the socket: the
	// loop no longer knows the identifier, and the server sees the client go.
	if err := c.loop.UnregisterConn(c.fd); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("UnregisterConn after Close: got %v, want ErrUnknownFD", err)
	}
	waitServerSawEOF(t, fake)
	if _, err := c.exec(ctx, "PING"); !errors.Is(err, ErrClosed) {
		t.Fatalf("exec after Close: got %v, want ErrClosed", err)
	}
}

func waitServerSawEOF(t *testing.T, fake *fakeRedis) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.conns)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake server never accepted the conn")
		}
		time.Sleep(time.Millisecond)
	}
	// The fake's serve loop returns (and closes its side) when it reads EOF.
	// A second dial would hide a leak, so probe the first conn directly.
	fake.mu.Lock()
	srv := fake.conns[0]
	fake.mu.Unlock()
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

func TestRedisNetConnServerHangUpFailsPendingAndLaterCommands(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "QUIT":
			// no reply: the server hangs up below
		default:
			writeSimple(w, "OK")
		}
	})
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialRedisConnNetConn(ctx, prov, Config{Addr: fake.Addr()}, 0)
	if err != nil {
		t.Fatalf("dialRedisConnNetConn: %v", err)
	}
	defer func() { _ = c.Close() }()

	fake.mu.Lock()
	for _, sc := range fake.conns {
		_ = sc.Close()
	}
	fake.mu.Unlock()

	// onClose marks the conn closed; commands after that fail promptly
	// instead of hanging on a conn nobody reads.
	deadline := time.Now().Add(5 * time.Second)
	for !c.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the conn never noticed the server hang up")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.exec(ctx, "PING"); err == nil {
		t.Fatal("exec on a conn whose server hung up succeeded")
	}
}

type noNetConnProvider struct{ engine.EventLoopProvider }

type noNetConnWorker struct{ engine.WorkerLoop }

func (p noNetConnProvider) NumWorkers() int { return 1 }
func (p noNetConnProvider) WorkerLoop(int) engine.WorkerLoop {
	return noNetConnWorker{}
}

func TestRedisNetConnDialErrorsCloseTheSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov engine.EventLoopProvider
		want string
	}{
		{"zero workers", zeroWorkerProvider{}, "0 workers"},
		{"worker cannot adopt a net.Conn", noNetConnProvider{}, "cannot adopt a net.Conn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := netConnTestServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := dialRedisConnNetConn(ctx, tc.prov, Config{Addr: fake.Addr()}, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("dial error = %v, want one containing %q", err, tc.want)
			}
			waitServerSawEOF(t, fake)
		})
	}
}

func TestRedisNetConnRegisterErrorClosesTheSocket(t *testing.T) {
	fake := netConnTestServer(t)
	l := netConnProvider(t)
	_ = l.Close() // a closed loop refuses RegisterNetConn with ErrLoopClosed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialRedisConnNetConn(ctx, l, Config{Addr: fake.Addr()}, 0)
	if !errors.Is(err, eventloop.ErrLoopClosed) {
		t.Fatalf("dial on a closed loop: got %v, want ErrLoopClosed", err)
	}
	waitServerSawEOF(t, fake)
}
