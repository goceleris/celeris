//go:build linux

package sse_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestClientLastEventIDSurvivesPeerBytes pins celeris#717, the SSE twin of
// celeris#714.
//
// On epoll and io_uring, c.Header returns a view of the engine's receive
// buffer. The middleware read Last-Event-ID before Context.Detach and kept
// that view in the Client for the whole stream, but the engine keeps
// receiving into the same buffer on a detached connection. Here the peer
// sends bytes after the stream is open (the engine then closes the
// connection), and the handler reads client.LastEventID() once its context
// is done: it must still be the header the client sent.
func TestClientLastEventIDSurvivesPeerBytes(t *testing.T) {
	var cells []sseCell
	iouring := false
	for _, kind := range sseNativeEngineKinds(t) {
		iouring = iouring || kind == celeris.IOUring
		cells = append(cells,
			sseCell{name: kind.String() + "/sync", engine: kind},
			sseCell{name: kind.String() + "/async", engine: kind, async: true},
		)
	}
	if !iouring && os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
		t.Fatal("io_uring is not available, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids dropping the io_uring cells")
	}
	cells = append(cells, sseCell{name: "std/sync", engine: celeris.Std})

	for _, cell := range cells {
		t.Run(cell.name, func(t *testing.T) {
			got := make(chan [2]string, 1)
			addr, shutdown := startSSEServer(t, cell.engine, cell.async, false, sse.New(sse.Config{
				// std notices a gone peer only when a write fails (its
				// request context is not cancelled once the peer has sent
				// bytes), so keep a short heartbeat for it.
				HeartbeatInterval: 100 * time.Millisecond,
				Handler: func(client *sse.Client) {
					before := client.LastEventID()
					<-client.Context().Done()
					got <- [2]string{before, client.LastEventID()}
				},
			}))
			defer shutdown()

			const n = 20
			wait := 2 * time.Second
			if cell.engine == celeris.Std {
				wait = 250 * time.Millisecond
			}
			wrongBefore, wrongAfter := 0, 0
			var samples []string
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("evt-%06d-%s", i, strings.Repeat("k", 24))
				conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				req := "GET /events HTTP/1.1\r\nHost: " + addr + "\r\nAccept: text/event-stream\r\nLast-Event-ID: " + id + "\r\n\r\n"
				if _, err := conn.Write([]byte(req)); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read response head: %v", err)
					}
					if line == "\r\n" {
						break
					}
				}
				// More bytes than the whole request, so every request byte
				// in the receive buffer is overwritten.
				_, _ = conn.Write([]byte(strings.Repeat("Z", 4*len(req))))
				// The native engines close a detached SSE connection that
				// sends; std does not, so bound the wait and then close.
				_ = conn.SetReadDeadline(time.Now().Add(wait))
				_, _ = io.Copy(io.Discard, br)
				_ = conn.Close()
				select {
				case v := <-got:
					if v[0] != id {
						wrongBefore++
					}
					if v[1] != id {
						wrongAfter++
						if len(samples) < 3 {
							samples = append(samples, fmt.Sprintf("want %q got %q", id, v[1]))
						}
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("conn %d: handler did not see its context end", i)
				}
			}
			t.Logf("C714SSE cell=%s streams=%d last_event_id_wrong_before=%d last_event_id_wrong_after=%d", cell.name, n, wrongBefore, wrongAfter)
			if wrongBefore+wrongAfter > 0 {
				t.Errorf("Client.LastEventID is not the header the client sent: wrong at stream start %d/%d, after the peer sent bytes %d/%d; samples %q",
					wrongBefore, n, wrongAfter, n, samples)
			}
		})
	}
}
