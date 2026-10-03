//go:build linux

package websocket

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestConnParamsAndPeerAddress pins celeris#721.
//
// Conn.Param must return the upgrade request's route params, and Conn.IP and
// Conn.RemoteAddr the peer's address, on every engine. Before the fix
// Conn.Param returned "" everywhere (nothing captured the params), and on
// epoll and io_uring (and Adaptive, which runs them) Conn.IP returned "" and
// Conn.RemoteAddr nil: only the hijack path, which std takes, had a net.Conn
// to ask.
//
// The params are request strings: on the native engines a view of the
// connection's receive buffer, which the engine keeps receiving WebSocket
// frames into. So the handler reports what the Conn holds twice, before the
// client sends a frame and after one longer than the whole upgrade request;
// both readings must match the request. The route has a named param and a
// catch-all.
//
// RemoteAddr must be a *net.TCPAddr with the peer's IP and port, as the
// hijack path's net.Conn gives, not only print like one. The arms run again
// on a dual-stack listener ("[::]:port"), where an IPv4 client is an AF_INET6
// peer with an IPv4-mapped address: std reports it as the IPv4 address, and
// the native engines as "[a.b.c.d]:port", which Conn must still answer as
// the *net.TCPAddr of the IPv4 peer. A host without dual-stack IPv6 skips
// those arms, unless CELERIS_REQUIRE_DUALSTACK=1 (CI) makes that a failure.
func TestConnParamsAndPeerAddress(t *testing.T) {
	type arm struct {
		name   string
		engine celeris.EngineType
		async  bool
	}
	arms := []arm{{"std", celeris.Std, false}, {"epoll", celeris.Epoll, false}, {"epoll-async", celeris.Epoll, true}}
	if ok, p := c714ProbeIOUring(); ok {
		arms = append(arms, arm{"io_uring", celeris.IOUring, false}, arm{"io_uring-async", celeris.IOUring, true})
	} else if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring arms", p.IOUringTier, p.KernelVersion)
	} else {
		t.Logf("io_uring tier=%s kernel=%s: io_uring arms not run", p.IOUringTier, p.KernelVersion)
	}
	arms = append(arms, arm{"adaptive", celeris.Adaptive, false}, arm{"adaptive-async", celeris.Adaptive, true})

	for _, listen := range []string{"127.0.0.1:0", "[::]:0"} {
		for _, a := range arms {
			name := a.name
			if listen != "127.0.0.1:0" {
				name = "dual-stack/" + a.name
			}
			t.Run(name, func(t *testing.T) {
				if listen != "127.0.0.1:0" {
					requireDualStack721(t)
				}
				testConnParamsAndPeerAddress(t, a.engine, a.async, listen)
			})
		}
	}
}

// testConnParamsAndPeerAddress runs one arm on a listener bound to listen.
func testConnParamsAndPeerAddress(t *testing.T, engine celeris.EngineType, async bool, listen string) {
	addr, stop := start721WSServer(t, listen, func() *celeris.Server {
		srv := celeris.New(celeris.Config{Engine: engine, AsyncHandlers: async})
		srv.GET("/ws/:room/*rest", New(Config{
			CheckOrigin: func(*celeris.Context) bool { return true },
			Handler: func(c *Conn) {
				if err := c.WriteMessage(TextMessage, []byte(connView721(c))); err != nil {
					return
				}
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
				_ = c.WriteMessage(TextMessage, []byte(connView721(c)))
				_, _, _ = c.ReadMessage()
			},
		}))
		return srv
	})
	defer stop()

	const conns = 20
	var wrong [5][2]int // [room|rest|ip|addr|tcp][before a frame|after a frame]
	var samples []string
	for i := 0; i < conns; i++ {
		cid := strconv.Itoa(100000 + i)
		before, after, local := roundTrip721(t, addr, cid)
		_, port, _ := net.SplitHostPort(local)
		want := splitView721("room=lobby-" + cid + "|rest=/a/b-" + cid + "|ip=127.0.0.1|addr=*net.TCPAddr tcp " + local + "|tcp=127.0.0.1 " + port)
		for phase, got := range []string{before, after} {
			g := splitView721(got)
			for part := range want {
				if g[part] != want[part] {
					wrong[part][phase]++
				}
			}
			if g != want && len(samples) < 4 {
				samples = append(samples, fmt.Sprintf("phase=%d want %q got %q", phase, want, g))
			}
		}
	}
	t.Logf("MW721 arm=%s conns=%d param_wrong=%d/%d catchall_wrong=%d/%d ip_wrong=%d/%d remoteaddr_wrong=%d/%d tcpaddr_wrong=%d/%d (before/after a frame)",
		t.Name(), conns, wrong[0][0], wrong[0][1], wrong[1][0], wrong[1][1], wrong[2][0], wrong[2][1], wrong[3][0], wrong[3][1], wrong[4][0], wrong[4][1])
	for part, what := range []string{
		`Conn.Param("room") does not return the upgrade request's param`,
		`Conn.Param("rest") does not return the upgrade request's catch-all`,
		"Conn.IP does not return the peer's IP",
		"Conn.RemoteAddr does not return the peer's address as a *net.TCPAddr",
		"Conn.RemoteAddr's TCPAddr does not hold the peer's IP and port",
	} {
		if wrong[part][0]+wrong[part][1] > 0 {
			t.Errorf("%s: wrong before a frame %d/%d, after a frame %d/%d; samples %q",
				what, wrong[part][0], conns, wrong[part][1], conns, samples)
		}
	}
}

// connView721 renders what the Conn reports about its upgrade request:
// RemoteAddr's type, network and string, and, for a *net.TCPAddr, its IP and
// port fields.
func connView721(c *Conn) string {
	ra, tcp := "<nil>", "<not a *net.TCPAddr>"
	if a := c.RemoteAddr(); a != nil {
		ra = fmt.Sprintf("%T %s %s", a, a.Network(), a.String())
		if ta, ok := a.(*net.TCPAddr); ok {
			tcp = ta.IP.String() + " " + strconv.Itoa(ta.Port)
		}
	}
	return "room=" + c.Param("room") + "|rest=" + c.Param("rest") + "|ip=" + c.IP() + "|addr=" + ra + "|tcp=" + tcp
}

func splitView721(s string) [5]string {
	var out [5]string
	for i := range out {
		part, rest, _ := strings.Cut(s, "|")
		out[i], s = part, rest
	}
	return out
}

// roundTrip721 upgrades one connection to /ws/lobby-<cid>/a/b-<cid>, reads
// the handler's first view, sends one masked text frame longer than the
// whole upgrade request, and reads the second view. It also returns the
// client's local address, which is the server's view of the peer.
func roundTrip721(t *testing.T, addr, cid string) (before, after, local string) {
	t.Helper()
	req := "GET /ws/lobby-" + cid + "/a/b-" + cid + " HTTP/1.1\r\nHost: " + addr +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade" +
		"\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := newUpgradeReader(t, conn)
	before = readTextPayload(t, br)
	if _, err := conn.Write(maskedTextFrame(len(req) + 64)); err != nil {
		t.Fatal(err)
	}
	after = readTextPayload(t, br)
	_, _ = conn.Write([]byte{0x88, 0x80, 1, 2, 3, 4}) // masked close, empty payload
	return before, after, conn.LocalAddr().String()
}

// requireDualStack721 skips a test on a host where "[::]:0" is not a
// dual-stack listener that an IPv4 client reaches (a kernel booted with
// ipv6.disable=1), or fails it when CELERIS_REQUIRE_DUALSTACK=1, as CI sets
// it, the pattern of the adaptive package's requireDualStack616.
func requireDualStack721(t *testing.T) {
	t.Helper()
	why := ""
	if ln, err := net.Listen("tcp", "[::]:0"); err != nil {
		why = fmt.Sprintf("net.Listen(\"[::]:0\"): %v", err)
	} else {
		port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
		if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+port, time.Second); err != nil {
			why = fmt.Sprintf("an IPv4 client cannot reach [::]:%s: %v", port, err)
		} else {
			_ = c.Close()
		}
		_ = ln.Close()
	}
	if why == "" {
		return
	}
	msg := why + ": the dual-stack arms need a dual-stack host"
	if os.Getenv("CELERIS_REQUIRE_DUALSTACK") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_DUALSTACK=1 forbids skipping")
	}
	t.Skip(msg)
}

// start721WSServer starts the server mk builds on a listener bound to listen
// ("127.0.0.1:0", or "[::]:0" for a dual-stack one) and returns the address
// to dial, on 127.0.0.1 either way, so the peer is an IPv4 client, and a
// shutdown closure. Like startC714WSServer, it fails at once with Start's
// error (celeris#706) and retries a start that fails only with ENOMEM.
func start721WSServer(t *testing.T, listen string, mk func() *celeris.Server) (string, func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := mk()
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			t.Fatalf("listen %s: %v", listen, err)
		}
		dial := net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
		err = wait721Ready(s, dial, done)
		if err == nil {
			return dial, func() { cancel(); <-done }
		}
		cancel()
		_ = ln.Close()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

// wait721Ready waits until the server publishes its address and dial
// accepts a connection, or start returns. The address comes first: the
// native engines close the listener they are handed and bind the port again
// (SO_REUSEPORT), and until then the handed listener's backlog accepts a
// dial that the engine then resets.
func wait721Ready(s *celeris.Server, dial string, done <-chan error) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("start returned before the server was ready")
			}
			return err
		default:
		}
		if s.Addr() == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if c, err := net.DialTimeout("tcp", dial, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("server not ready within 30s")
}

// newUpgradeReader reads the 101 response's status line and headers.
func newUpgradeReader(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		t.Fatalf("upgrade: status %q err %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			return br
		}
	}
}
