//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
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

// getStatus is getOnce without t.Fatal, for checks that report every family.
func getStatus(addr string) (int, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		return 0, err
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// netListenAddr is what net.Listen makes of in, which is what New handed the
// sub-engines before celeris#616.
func netListenAddr(t *testing.T, in string) string {
	t.Helper()
	ln, err := net.Listen("tcp", in)
	if err != nil {
		t.Fatalf("net.Listen(%q): %v", in, err)
	}
	a := ln.Addr().String()
	_ = ln.Close()
	return a
}

// TestAdaptivePortHeldWildcard616 is the New-to-Listen steal on the default
// address form: a wildcard ":0", which New makes the dual-stack "[::]:PORT"
// wherever Go does. In the gap every IPv4 and IPv6 socket on the port must be
// refused, so the hold must cover the IPv4 half of the dual-stack port too
// (a hold with IPV6_V6ONLY set would leave it to an IPv4 thief), and the
// engine must then start and serve both families.
func TestAdaptivePortHeldWildcard616(t *testing.T) {
	for _, se := range startEngines {
		t.Run(se.env, func(t *testing.T) {
			if se.want == engine.IOUring && !probe.Probe().IOUringTier.Available() {
				t.Skip("io_uring unavailable here: cannot start adaptive on it")
			}
			if a := netListenAddr(t, ":0"); !strings.HasPrefix(a, "[::]:") {
				t.Fatalf("net.Listen(\":0\") gives %s here, not a dual-stack [::]:PORT: this test needs a dual-stack host", a)
			}
			t.Setenv("CELERIS_ADAPTIVE_START", se.env)
			e, err := New(resource.Config{Addr: ":0", Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
			if err != nil {
				t.Fatalf("adaptive.New: %v", err)
			}
			if e.startType != se.want {
				_ = e.Shutdown(context.Background())
				t.Skipf("adaptive chose %v as its start engine, not %v (io_uring could not be built here)", e.startType, se.want)
			}
			addr := e.cfg.Addr
			_, ps, err := net.SplitHostPort(addr)
			if err != nil || !strings.HasPrefix(addr, "[::]:") || ps == "0" {
				_ = e.Shutdown(context.Background())
				t.Fatalf("New(\":0\") handed the sub-engines %q, want [::]:PORT with the port decided", addr)
			}
			thieves := []struct {
				name  string
				steal func() (io.Closer, error)
			}{
				{"go-tcp4-0.0.0.0", func() (io.Closer, error) { return net.Listen("tcp4", "0.0.0.0:"+ps) }},
				{"go-tcp4-127.0.0.1", func() (io.Closer, error) { return net.Listen("tcp4", "127.0.0.1:"+ps) }},
				{"go-tcp-wildcard", func() (io.Closer, error) { return net.Listen("tcp", ":"+ps) }},
				{"go-tcp6-::1", func() (io.Closer, error) { return net.Listen("tcp6", "[::1]:"+ps) }},
				{"bare-0.0.0.0", func() (io.Closer, error) { return bareListen("0.0.0.0:" + ps) }},
				{"bare-127.0.0.1", func() (io.Closer, error) { return bareListen("127.0.0.1:" + ps) }},
			}
			refused := 0
			var report []string
			for _, th := range thieves {
				c, err := th.steal()
				switch {
				case err == nil:
					_ = c.Close()
					report = append(report, th.name+"=BOUND")
					t.Errorf("celeris#616: %s bound port %s between New and Listen: the hold on %s does not cover it", th.name, ps, addr)
				case errors.Is(err, syscall.EADDRINUSE):
					refused++
					report = append(report, th.name+"=EADDRINUSE")
				default:
					report = append(report, th.name+"=ERR")
					t.Errorf("thief %s failed with %v, not EADDRINUSE", th.name, err)
				}
			}
			stop, lerr := startAndServe(t, e)
			if lerr != nil {
				_ = e.Shutdown(context.Background())
				t.Fatalf("the engine did not start on %s: %v", addr, lerr)
			}
			v4, err4 := getStatus("127.0.0.1:" + ps)
			v6, err6 := getStatus("[::1]:" + ps)
			stop()
			runtime.KeepAlive(e)
			t.Logf("celeris616 wildcard: start=%s addr=%s thieves=[%s] refused=%d/%d started=true v4=%d v6=%d",
				se.env, addr, strings.Join(report, " "), refused, len(thieves), v4, v6)
			if v4 != 200 || v6 != 200 {
				t.Errorf("the engine on %s answered GET with v4=%d (%v) v6=%d (%v), want 200 on both families", addr, v4, err4, v6, err6)
			}
		})
	}
}

// countTimeWait counts the TIME_WAIT sockets (state 06) whose local port is
// port.
func countTimeWait(port int) int {
	n := 0
	suffix := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			if fs := strings.Fields(line); len(fs) > 3 && strings.HasSuffix(fs[1], suffix) && fs[3] == "06" {
				n++
			}
		}
	}
	return n
}

// leaveTimeWait runs what an ordinary Go server leaves behind when it stops:
// a net.Listen listener on ":0" (SO_REUSEADDR, no SO_REUSEPORT: net/http, or
// the std engine) serves one connection, closes it first, so the server's end
// stays on the port in TIME_WAIT, and closes. It returns the port.
//
// It asserts that this state is the one the exclusive hold cannot be bound
// in: a socket with SO_REUSEPORT and without SO_REUSEADDR, bound the way that
// hold binds, gets EADDRINUSE on the port.
func leaveTimeWait(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("predecessor listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial predecessor: %v", err)
	}
	s := <-accepted
	if s == nil {
		t.Fatal("predecessor accepted nothing")
	}
	_ = s.Close() // the server closes first: its end goes to TIME_WAIT
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.ReadAll(c)
	_ = c.Close()
	_ = ln.Close()
	deadline := time.Now().Add(3 * time.Second)
	for countTimeWait(port) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if countTimeWait(port) == 0 {
		t.Fatalf("fixture: no TIME_WAIT socket on port %d", port)
	}

	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	if err := unix.Bind(fd, &unix.SockaddrInet6{Port: port}); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("fixture: an SO_REUSEPORT-only socket bound beside the TIME_WAIT sockets on port %d (err %v): the state under test was not made", port, err)
	}
	return port
}

// TestAdaptivePortHeldBesideTimeWait616: the port New is given still has the
// TIME_WAIT sockets an ordinary Go server left on it, the state of a restart
// that replaces such a server within a minute or so. The kernel refuses the
// exclusive hold there, as it refuses any socket without SO_REUSEADDR, so New
// must take the shared hold instead, and:
//
//   - hand the sub-engines exactly the address net.Listen gives for the same
//     input (before celeris#616 New handed them that address; a New that gave
//     up on the hold and kept the literal address made ":P" an IPv4-only
//     0.0.0.0:P and "localhost:P" an address the sub-engines cannot parse);
//   - still hold a wildcard port against a socket without SO_REUSEADDR on an
//     address the TIME_WAIT sockets do not cover (127.0.0.2);
//   - start on the port and serve it on every family that address has.
func TestAdaptivePortHeldBesideTimeWait616(t *testing.T) {
	forms := []struct {
		name, format string
		wildcard     bool
	}{
		{"wildcard", ":%d", true},
		{"0.0.0.0", "0.0.0.0:%d", true},
		{"[::]", "[::]:%d", true},
		{"127.0.0.1", "127.0.0.1:%d", false},
		{"localhost", "localhost:%d", false},
	}
	for _, se := range startEngines {
		for _, f := range forms {
			t.Run(se.env+"/"+f.name, func(t *testing.T) {
				if se.want == engine.IOUring && !probe.Probe().IOUringTier.Available() {
					t.Skip("io_uring unavailable here: cannot start adaptive on it")
				}
				t.Setenv("CELERIS_ADAPTIVE_START", se.env)
				port := leaveTimeWait(t)
				ps := strconv.Itoa(port)
				tw := countTimeWait(port)
				in := fmt.Sprintf(f.format, port)
				want := netListenAddr(t, in)

				e, err := New(resource.Config{Addr: in, Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
				if err != nil {
					t.Fatalf("adaptive.New(%q): %v", in, err)
				}
				if e.startType != se.want {
					_ = e.Shutdown(context.Background())
					t.Skipf("adaptive chose %v as its start engine, not %v (io_uring could not be built here)", e.startType, se.want)
				}
				handed := e.cfg.Addr
				if handed != want {
					t.Errorf("New(%q) handed the sub-engines %q; net.Listen gives %q, which is what they got before celeris#616", in, handed, want)
				}

				thief := "-"
				if f.wildcard {
					c, err := bareListen("127.0.0.2:" + ps)
					switch {
					case err == nil:
						_ = c.Close()
						thief = "BOUND"
						t.Errorf("celeris#616: a socket without SO_REUSEADDR bound 127.0.0.2:%s between New and Listen: New did not hold %s", ps, handed)
					case errors.Is(err, syscall.EADDRINUSE):
						thief = "EADDRINUSE"
					default:
						_ = e.Shutdown(context.Background())
						t.Fatalf("thief on 127.0.0.2:%s failed with %v, not EADDRINUSE", ps, err)
					}
				}

				stop, lerr := startAndServe(t, e)
				if lerr != nil {
					_ = e.Shutdown(context.Background())
					t.Fatalf("the engine did not start on %q (New was given %q): %v", handed, in, lerr)
				}
				if got := e.Addr().(*net.TCPAddr).Port; got != port {
					t.Errorf("engine serves on port %d, want %d", got, port)
				}
				v4, err4 := getStatus("127.0.0.1:" + ps)
				v6, v6want := 0, strings.HasPrefix(want, "[::]:")
				var err6 error
				if v6want {
					v6, err6 = getStatus("[::1]:" + ps)
				}
				stop()

				// The thief works on a free port: with the engine gone it binds
				// 127.0.0.2, which shows its refusal above was the hold (an
				// io_uring engine's port takes a few ms to come free, #896).
				if f.wildcard {
					if _, err := stealWithin(portThief{"bare-127.0.0.2", bareListen}, "127.0.0.2:"+ps, 2*time.Second); err != nil {
						t.Errorf("2s after the engine stopped the thief still could not bind 127.0.0.2:%s: %v", ps, err)
					}
				}
				runtime.KeepAlive(e)
				t.Logf("celeris616 time-wait: start=%s form=%s time_wait=%d exclusive_refused=true net_listen=%s handed=%s thief=%s started=true v4=%d v6=%d",
					se.env, f.name, tw, want, handed, thief, v4, v6)
				if v4 != 200 {
					t.Errorf("GET 127.0.0.1:%s = %d (%v), want 200", ps, v4, err4)
				}
				if v6want && v6 != 200 {
					t.Errorf("GET [::1]:%s = %d (%v), want 200: %s is dual-stack", ps, v6, err6, want)
				}
			})
		}
	}
}

// TestAdaptiveTimeWaitStealFailsLoudly616 pins what is left of the gap beside
// those TIME_WAIT sockets: a socket WITH SO_REUSEADDR (a Go listener) can
// share a port with them and with the shared hold while none of them listen,
// so the kernel lets it take the port. It must still come out as before
// celeris#616: Listen fails with EADDRINUSE, and the engine never starts on
// some other address. (If the kernel refuses the thief, the engine must
// start.)
func TestAdaptiveTimeWaitStealFailsLoudly616(t *testing.T) {
	for _, se := range startEngines {
		t.Run(se.env, func(t *testing.T) {
			if se.want == engine.IOUring && !probe.Probe().IOUringTier.Available() {
				t.Skip("io_uring unavailable here: cannot start adaptive on it")
			}
			t.Setenv("CELERIS_ADAPTIVE_START", se.env)
			port := leaveTimeWait(t)
			ps := strconv.Itoa(port)
			e, err := New(resource.Config{Addr: ":" + ps, Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
			if err != nil {
				t.Fatalf("adaptive.New: %v", err)
			}
			if e.startType != se.want {
				_ = e.Shutdown(context.Background())
				t.Skipf("adaptive chose %v as its start engine, not %v (io_uring could not be built here)", e.startType, se.want)
			}
			thief, terr := net.Listen("tcp4", "127.0.0.2:"+ps)
			stop, lerr := startAndServe(t, e)
			if stop != nil {
				stop()
			} else {
				_ = e.Shutdown(context.Background())
			}
			if thief != nil {
				_ = thief.Close()
			}
			t.Logf("celeris616 time-wait go-listener steal: start=%s addr=%s steal_err=%v listen_err=%v", se.env, e.cfg.Addr, terr, lerr)
			switch {
			case terr == nil && lerr == nil:
				t.Errorf("a Go listener holds 127.0.0.2:%s and the engine still started on %s: it is not serving the port it was given", ps, e.cfg.Addr)
			case terr == nil && !errors.Is(lerr, syscall.EADDRINUSE) && !strings.Contains(lerr.Error(), "address already in use"):
				t.Errorf("the port was taken and Listen failed with %v, want EADDRINUSE", lerr)
			case terr != nil && lerr != nil:
				t.Errorf("nothing took the port (steal: %v) and the engine did not start: %v", terr, lerr)
			}
		})
	}
}

// TestAdaptiveNoHoldOnlyWhereNetListenFails616: when the port is already
// taken by a listener that does not share it, neither hold can be bound, and
// New must hand the sub-engines the address unchanged, which is what it did
// before celeris#616 because net.Listen fails there too. The start engine's
// bind then reports the conflict.
func TestAdaptiveNoHoldOnlyWhereNetListenFails616(t *testing.T) {
	t.Setenv("CELERIS_ADAPTIVE_START", "epoll")
	for _, f := range []struct{ owner, in string }{
		{":0", ":%d"},
		{"127.0.0.1:0", "127.0.0.1:%d"},
	} {
		t.Run(f.in, func(t *testing.T) {
			owner, err := net.Listen("tcp", f.owner)
			if err != nil {
				t.Fatalf("owner listen: %v", err)
			}
			defer func() { _ = owner.Close() }()
			in := fmt.Sprintf(f.in, owner.Addr().(*net.TCPAddr).Port)
			if ln, err := net.Listen("tcp", in); err == nil {
				_ = ln.Close()
				t.Fatalf("fixture: net.Listen(%q) succeeded beside the owner", in)
			}
			e, err := New(resource.Config{Addr: in, Protocol: engine.HTTP1, Resources: resource.Resources{Workers: 2}}, respHandler{}, nil)
			if err != nil {
				t.Fatalf("adaptive.New(%q): %v", in, err)
			}
			_, lerr := startAndServe(t, e)
			_ = e.Shutdown(context.Background())
			t.Logf("celeris616 taken port: in=%q handed=%q listen_err=%v", in, e.cfg.Addr, lerr)
			if e.cfg.Addr != in {
				t.Errorf("New(%q) handed the sub-engines %q on a port net.Listen cannot bind; before celeris#616 they got %q", in, e.cfg.Addr, in)
			}
			if lerr == nil || !strings.Contains(lerr.Error(), "address already in use") {
				t.Errorf("Listen on a taken port returned %v, want the start engine's EADDRINUSE", lerr)
			}
		})
	}
}
