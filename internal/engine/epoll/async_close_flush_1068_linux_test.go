//go:build linux

package epoll

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1068 (and the TestStreamedResponseIsDelivered/*/async-route/close
// item of #890): an async-route response that ends its connection while its
// remainder is still queued was cut short, at the same byte count each time
// (the bytes the socket buffers took at the first flush: 2634119 of a
// 3 MiB body, 2681685 of an 8 MiB one), and the client saw EOF.
//
// serveAsync, after a request that ends the conn (Connection: close, an error,
// a refused write), declared the conn closed (asyncClosed) and only then took
// asyncInMu to mark its goroutine gone (endDispatch). A drainDetachQueue that
// read the conn between the two found a conn declared closed whose goroutine
// was still counted as running, took that for a close requested from outside
// while a handler runs, and called closeConn: the handler's detachMu was free
// (the goroutine had released it after its flush), so closeConn went through
// and closed the descriptor with the rest of the response unsent. The entry
// that made the loop look was the one the goroutine enqueued for the remainder
// of its partial flush, before it declared the close.
//
// The test holds the goroutine in that gap, with the response's remainder
// queued, and waits for the loop to look. With the gap closed the loop cannot
// look until the goroutine has finished (the wait ends on its timeout), and
// sends the remainder before it closes.

func TestAsyncRouteCloseSendsTheQueuedRemainder1068(t *testing.T) {
	for _, tc := range []struct {
		name string
		// reqs requests are sent in one packet, header the lines after Host.
		reqs   int
		header string
		body   int
		// closesAtBoundary: the conn ends after whole responses only (the
		// back-pressure refusal of celeris#761 drops the responses past the
		// cap), so any number of them is right, but each one that is read
		// must be whole, and then the stream ends (TestBackloggedPeerIsClosed
		// takes an EOF in the next response's head as that end too).
		closesAtBoundary bool
	}{
		// TestStreamedResponseIsDelivered/*/async-route/close: one response
		// larger than the socket buffers, then Connection: close.
		{"connection-close", 1, "Connection: close\r\n", 8 << 20, false},
		// TestBackloggedPeerIsClosed/*/async-*: four pipelined 3 MiB responses
		// for a client that reads nothing; the cap refuses one past 4 MiB.
		{"refused-write", 4, "", 3 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				looked   = make(chan struct{})
				lookOnce sync.Once
				held     = make(chan struct{})
				heldOnce sync.Once
			)
			exitHook := func(*connState) {
				select {
				case <-looked:
				case <-time.After(500 * time.Millisecond):
				}
				heldOnce.Do(func() { close(held) })
			}
			drainHook := func(*connState) { lookOnce.Do(func() { close(looked) }) }
			testHookAsyncExitClosed.Store(&exitHook)
			testHookDrainAsyncClosed.Store(&drainHook)
			t.Cleanup(func() {
				testHookAsyncExitClosed.Store(nil)
				testHookDrainAsyncClosed.Store(nil)
			})

			_, addr := startE2E885(t, true, newPathHandler885(tc.body), func(*resource.Config) {})
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if _, err := io.WriteString(c, strings.Repeat("GET /big HTTP/1.1\r\nHost: x\r\n"+tc.header+"\r\n", tc.reqs)); err != nil {
				t.Fatal(err)
			}
			// The client reads nothing until the goroutine has been held and
			// let go, so the response cannot fit the socket buffers and its
			// remainder is queued at every point of the hold.
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("the dispatch goroutine never reached the exit of a request that ends its conn")
			}
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
			br := bufio.NewReaderSize(c, 64<<10)
			whole := 0
			for {
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					if whole > 0 && tc.closesAtBoundary && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
						break
					}
					t.Fatalf("response %d: %v (looked=%v)", whole+1, err, isClosed1068(looked))
				}
				n, err := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil || n != int64(tc.body) {
					t.Fatalf("response %d: received %d of %d body bytes (%v): the loop closed the conn with the rest of the response unsent while the goroutine was still on its way out (looked=%v)",
						whole+1, n, tc.body, err, isClosed1068(looked))
				}
				whole++
				if !tc.closesAtBoundary || whole == tc.reqs {
					break
				}
			}
			if tc.closesAtBoundary && whole == tc.reqs {
				t.Fatalf("all %d responses were sent to a client that read nothing: the cap did not refuse one", tc.reqs)
			}
		})
	}
}

func isClosed1068(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
