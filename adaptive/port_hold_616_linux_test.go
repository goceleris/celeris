//go:build linux

package adaptive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// celeris#616: New picks the port both sub-engines will serve, and the start
// engine binds it only in Listen. The tests here take the port in that gap,
// the way any other socket on the host can, and need the engine to start
// anyway, on the port New picked. They use nothing but New, Listen, Addr and
// Shutdown, so they build and run against the code before the fix as well.

// portThief binds and listens on an address with the options of one kind of
// foreign socket.
type portThief struct {
	name  string
	steal func(addr string) (io.Closer, error)
}

var portThieves = []portThief{
	// An ordinary Go listener: SO_REUSEADDR set, SO_REUSEPORT not.
	{"go-listener", func(addr string) (io.Closer, error) { return net.Listen("tcp", addr) }},
	// A bare socket with neither option.
	{"bare-socket", bareListen},
}

func bareListen(addr string) (io.Closer, error) {
	ta, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ip4 := ta.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("bareListen takes an IPv4 address, got %s", addr)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	sa := &unix.SockaddrInet4{Port: ta.Port}
	copy(sa.Addr[:], ip4)
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 16); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return fdCloser(fd), nil
}

// stealWithin retries th until it binds addr or limit has passed, closes what
// it bound, and returns how long that took.
func stealWithin(th portThief, addr string, limit time.Duration) (time.Duration, error) {
	start := time.Now()
	for {
		c, err := th.steal(addr)
		if err == nil {
			_ = c.Close()
			return time.Since(start), nil
		}
		if time.Since(start) > limit {
			return time.Since(start), err
		}
		time.Sleep(time.Millisecond)
	}
}

type fdCloser int

func (c fdCloser) Close() error { return unix.Close(int(c)) }

// startEngines are the two engines adaptive can start on. The port is bound
// by the start engine, so the window is the same on both and both are run.
var startEngines = []struct {
	env  string
	want engine.EngineType
}{
	{"epoll", engine.Epoll},
	{"iouring", engine.IOUring},
}

// newGapEngine builds an adaptive engine on 127.0.0.1:0 that starts on the
// requested engine, and returns it with the address New decided on.
func newGapEngine(t *testing.T, startEnv string, want engine.EngineType) (*Engine, string, int) {
	t.Helper()
	if want == engine.IOUring && !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable here: cannot start adaptive on it")
	}
	t.Setenv("CELERIS_ADAPTIVE_START", startEnv)
	e, err := New(resource.Config{
		Addr:      "127.0.0.1:0",
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, respHandler{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err)
	}
	if e.startType != want {
		_ = e.Shutdown(context.Background())
		t.Skipf("adaptive chose %v as its start engine, not %v (io_uring could not be built here)", e.startType, want)
	}
	addr := e.cfg.Addr
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("New left Addr=%q, not a host:port: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		t.Fatalf("New left Addr=%q: the port was not decided (port %q)", addr, portStr)
	}
	return e, addr, port
}

// startAndServe runs Listen and waits for the engine to publish an address. It
// returns the Listen error if the engine failed to start, and otherwise a stop
// func that cancels Listen and waits for it.
func startAndServe(t *testing.T, e *Engine) (stop func(), err error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	deadline := time.After(25 * time.Second)
	for e.Addr() == nil {
		select {
		case lerr := <-done:
			cancel()
			if lerr == nil {
				lerr = errors.New("Listen returned nil without publishing an address")
			}
			return nil, lerr
		case <-deadline:
			cancel()
			<-done
			return nil, errors.New("no address published within 25s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return func() {
		cancel()
		if lerr := <-done; lerr != nil {
			t.Errorf("Listen returned %v after a clean stop", lerr)
		}
		_ = e.Shutdown(context.Background())
	}, nil
}

// TestAdaptivePortHeldFromNewToListen616 is the celeris#616 regression. It
// takes the port New picked after New has returned and before Listen is
// called. That must be refused with EADDRINUSE, and the engine must then
// start on that port and serve on it. Before the fix the port was closed
// again inside New: the thief got it, and Listen failed with EADDRINUSE.
//
// Once the engine has stopped, the same thief must get the port: that shows
// the thief can bind a free port (so its refusal above was the hold, not a
// broken thief) and that nothing of the engine's still holds the port.
func TestAdaptivePortHeldFromNewToListen616(t *testing.T) {
	for _, se := range startEngines {
		for _, th := range portThieves {
			t.Run(se.env+"/"+th.name, func(t *testing.T) {
				e, addr, port := newGapEngine(t, se.env, se.want)

				// The gap: New has returned, Listen has not been called.
				stolen, stealErr := th.steal(addr)
				t.Logf("celeris616 gap: start=%s thief=%s addr=%s steal_err=%v", se.env, th.name, addr, stealErr)
				if stealErr == nil {
					t.Errorf("celeris#616: %s bound %s between New and Listen: New did not hold the port it picked", th.name, addr)
				} else if !errors.Is(stealErr, syscall.EADDRINUSE) {
					_ = e.Shutdown(context.Background())
					t.Fatalf("thief %s failed with %v, not EADDRINUSE: the steal was not refused by a hold on the port", th.name, stealErr)
				}

				stop, lerr := startAndServe(t, e)
				if stolen != nil {
					_ = stolen.Close()
				}
				if lerr != nil {
					_ = e.Shutdown(context.Background())
					t.Fatalf("celeris#616: the engine did not start on %s after the steal attempt: %v", addr, lerr)
				}
				got := e.Addr().(*net.TCPAddr)
				if got.Port != port {
					t.Errorf("engine serves on port %d, want the port New picked, %d", got.Port, port)
				}
				if typ := e.ActiveEngine().Type(); typ != se.want {
					t.Errorf("engine started on %v, want %v", typ, se.want)
				}
				if code := getOnce(t, addr); code != 200 {
					t.Errorf("GET %s = %d, want 200", addr, code)
				}
				stop()

				// After the stop the port is taken back with the Go listener
				// whichever thief ran above: the request just served can leave
				// the server's end of its connection in TIME_WAIT on this port,
				// which refuses a socket without SO_REUSEADDR and not one with
				// it. (TestAdaptiveShutdownWithoutListenFreesPort616 shows the
				// bare socket binding a freed port.) An io_uring worker closes
				// its listen socket while its ring still holds a reference to
				// it, from the multishot accept, and the kernel drops that
				// reference only when it tears the ring down, after Listen has
				// returned (measured at ~2 ms), so the port gets a bounded time
				// to come free.
				waited, err := stealWithin(portThieves[0], addr, 2*time.Second)
				if err != nil {
					t.Errorf("2s after the engine stopped, %s still could not bind %s: %v (the port is still held, or the thief cannot bind at all)", portThieves[0].name, addr, err)
				}
				// The engine must stay reachable until here: a hold dropped
				// with the engine is closed by its finalizer, which would
				// free the port for the check above for the wrong reason.
				runtime.KeepAlive(e)
				t.Logf("celeris616 result: start=%s thief=%s steal_in_gap_refused=%v started=%v port=%d free_after_stop_in=%v", se.env, th.name, stealErr != nil, lerr == nil, got.Port, waited)
			})
		}
	}
}

// TestAdaptivePortHoldRefusesDials616 pins what the hold must not do: accept a
// connection. Between New and Listen a dial to the port is refused, as it was
// when nothing held the port. A socket that listened would take the dial into
// a backlog nobody accepts from, and, once the start engine's sockets joined
// it in the port's reuseport group, a share of every later connection.
func TestAdaptivePortHoldRefusesDials616(t *testing.T) {
	for _, se := range startEngines {
		t.Run(se.env, func(t *testing.T) {
			e, addr, _ := newGapEngine(t, se.env, se.want)
			defer func() { _ = e.Shutdown(context.Background()) }()

			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err == nil {
				_ = c.Close()
				t.Fatalf("a dial to %s between New and Listen was accepted: something on the port is listening", addr)
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("a dial to %s between New and Listen failed with %v, want ECONNREFUSED", addr, err)
			}
			t.Logf("celeris616 dial in the gap: start=%s addr=%s err=%v", se.env, addr, err)
		})
	}
}

// TestAdaptiveShutdownWithoutListenFreesPort616: an engine that is built and
// shut down without ever being started must give its port back.
func TestAdaptiveShutdownWithoutListenFreesPort616(t *testing.T) {
	e, addr, _ := newGapEngine(t, "epoll", engine.Epoll)
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// No sub-engine ever bound the port, so nothing but the hold can keep it:
	// it must be free at once.
	for _, th := range portThieves {
		c, err := th.steal(addr)
		if err != nil {
			t.Errorf("after Shutdown of an engine that never listened, %s could not bind %s: %v", th.name, addr, err)
			continue
		}
		_ = c.Close()
	}
	runtime.KeepAlive(e) // see TestAdaptivePortHeldFromNewToListen616
}

// TestAdaptiveAddrMatchesNetListen616 pins the address New hands the
// sub-engines. It must be the one net.Listen gives for the same input, which
// is what New handed them before celeris#616: the same host, the same family
// (a wildcard host is dual-stack IPv6 wherever Go makes it so), only a
// different port number.
func TestAdaptiveAddrMatchesNetListen616(t *testing.T) {
	t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
	for _, in := range []string{":0", "0.0.0.0:0", "[::]:0", "127.0.0.1:0", "[::1]:0", "localhost:0"} {
		t.Run(in, func(t *testing.T) {
			ln, err := net.Listen("tcp", in)
			if err != nil {
				t.Skipf("net.Listen(%q) fails on this host: %v", in, err)
			}
			want := ln.Addr().(*net.TCPAddr)
			_ = ln.Close()

			e, err := New(resource.Config{Addr: in, Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
			if err != nil {
				t.Fatalf("adaptive.New(%q): %v", in, err)
			}
			defer func() { _ = e.Shutdown(context.Background()) }()
			got, err := net.ResolveTCPAddr("tcp", e.cfg.Addr)
			if err != nil {
				t.Fatalf("New left Addr=%q: %v", e.cfg.Addr, err)
			}
			wantHost := (&net.TCPAddr{IP: want.IP, Zone: want.Zone}).String()
			gotHost := (&net.TCPAddr{IP: got.IP, Zone: got.Zone}).String()
			t.Logf("celeris616 addr form %q: net.Listen %s, New %s", in, want, e.cfg.Addr)
			if gotHost != wantHost {
				t.Errorf("New(%q) handed the sub-engines host %s, net.Listen gives %s", in, gotHost, wantHost)
			}
			if got.Port == 0 {
				t.Errorf("New(%q) left the port undecided: %s", in, e.cfg.Addr)
			}
		})
	}
}
