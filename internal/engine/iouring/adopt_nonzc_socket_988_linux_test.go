//go:build linux

package iouring

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// adoptShapes988 are the four shapes of the bare-metal
// TestIouringAdoptCarriedLarge{Response,Close}{Sync,Async}908 failures.
var adoptShapes988 = []struct {
	name         string
	async, close bool
}{
	{"ResponseSync", false, false}, {"CloseSync", false, true},
	{"ResponseAsync", true, false}, {"CloseAsync", true, true},
}

// runAdoptBig988 adopts fd carrying a pipelined GET /big (32 MiB, larger than
// the socket buffers) and reads the response through client: the whole body,
// then a served follow-up (keep-alive) or EOF (Connection: close).
func runAdoptBig988(t *testing.T, async, closeConn bool, client net.Conn, fd int) {
	t.Helper()
	e, _ := start908(t, async, false)
	req := "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"
	if closeConn {
		req = "GET /big HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	}
	if err := e.AdoptConn(fd, engine.Carryover{RemoteAddr: "127.0.0.1:1", Buffered: []byte(req)}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the replay flush what the socket takes before reading
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the /big response head: %v (the conn was closed with 0 bytes sent)", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || n != big908 {
		t.Fatalf("read %d of %d body bytes: %v", n, big908, err)
	}
	if closeConn {
		if !hasEOF908(client, br, 3*time.Second) {
			t.Errorf("the conn stayed open after a Connection: close response")
		}
		return
	}
	if _, err := client.Write([]byte("GET /after HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write the follow-up: %v", err)
	}
	if code, body, err := readBody543(client, br, 3*time.Second); err != nil || code != 200 || body != "/after" {
		t.Errorf("follow-up after /big: %d %q, %v", code, body, err)
	}
}

// TestIouringAdoptNonZCSocket988 is the container-reproducible form of the
// four bare-metal TestIouringAdoptCarriedLarge{Response,Close}{Sync,Async}908
// failures: the adopted fd is a socket whose struct socket lacks
// SOCK_SUPPORT_ZC (AF_UNIX here; on the cluster a TCP conn accepted through
// Go's default MPTCP listener, which the unchanged 908 tests adopt), so the
// replay's 32 MiB SEND_ZC completes -EOPNOTSUPP|F_MORE then NOTIF. The client
// must still get the whole response, then a served follow-up (Response) or EOF
// (Close).
func TestIouringAdoptNonZCSocket988(t *testing.T) {
	for _, tc := range adoptShapes988 {
		t.Run(tc.name, func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Skipf("socketpair: %v", err)
			}
			if err := unix.SetNonblock(pair[0], true); err != nil {
				t.Fatal(err)
			}
			f := os.NewFile(uintptr(pair[1]), "client")
			client, err := net.FileConn(f)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			runAdoptBig988(t, tc.async, tc.close, client, pair[0])
		})
	}
}
