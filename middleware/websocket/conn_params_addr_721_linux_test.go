//go:build linux

package websocket

import (
	"bufio"
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

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			addr, stop := startC714WSServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
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
			var wrong [4][2]int // [room|rest|ip|addr][before a frame|after a frame]
			var samples []string
			for i := 0; i < conns; i++ {
				cid := strconv.Itoa(100000 + i)
				before, after, local := roundTrip721(t, addr, cid)
				want := splitView721("room=lobby-" + cid + "|rest=/a/b-" + cid + "|ip=127.0.0.1|addr=tcp " + local)
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
			t.Logf("MW721 arm=%s conns=%d param_wrong=%d/%d catchall_wrong=%d/%d ip_wrong=%d/%d remoteaddr_wrong=%d/%d (before/after a frame)",
				a.name, conns, wrong[0][0], wrong[0][1], wrong[1][0], wrong[1][1], wrong[2][0], wrong[2][1], wrong[3][0], wrong[3][1])
			for part, what := range []string{
				`Conn.Param("room") does not return the upgrade request's param`,
				`Conn.Param("rest") does not return the upgrade request's catch-all`,
				"Conn.IP does not return the peer's IP",
				"Conn.RemoteAddr does not return the peer's address",
			} {
				if wrong[part][0]+wrong[part][1] > 0 {
					t.Errorf("%s: wrong before a frame %d/%d, after a frame %d/%d; samples %q",
						what, wrong[part][0], conns, wrong[part][1], conns, samples)
				}
			}
		})
	}
}

// connView721 renders what the Conn reports about its upgrade request.
func connView721(c *Conn) string {
	ra := "<nil>"
	if a := c.RemoteAddr(); a != nil {
		ra = a.Network() + " " + a.String()
	}
	return "room=" + c.Param("room") + "|rest=" + c.Param("rest") + "|ip=" + c.IP() + "|addr=" + ra
}

func splitView721(s string) [4]string {
	var out [4]string
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
