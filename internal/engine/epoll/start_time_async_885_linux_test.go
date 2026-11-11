//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#885 (the epoll twin of the io_uring finding of celeris#884): the stamp
// a request's start time is read from, H1State.NowNs, is written by the loop
// when it reads a conn's bytes itself. The requests of a conn that the
// dispatch goroutine reads (promoted to async) never saw a fresh one: they carried the instant of the conn's last inline read, so
// Context.StartTime and the duration handed to the metrics collector grew by
// the whole idle gap, and so did a detached handler's elapsed time. The same
// held for the pipelined requests that follow a body received straight into
// H1State.bodyBuf: stamped with the read of the headers of the body's request.

// idleGap885 separates the requests of the tests below; staleLimit885 is what a
// request's start may lag its handler by: well over a loaded runner's
// scheduling noise, well under the gap.
const (
	idleGap885    = 400 * time.Millisecond
	staleLimit885 = 200 * time.Millisecond
)

type startRec885 struct {
	path string
	age  time.Duration // the handler's clock minus the stream's start stamp
	zero bool          // no stamp: the router reads the clock itself (celeris HandleStream)
}

// startHandler885 reports, for every request, how old its start stamp is when
// its handler runs. Paths starting with /async are async routes.
type startHandler885 struct{ recs chan startRec885 }

func (h *startHandler885) HandleStream(_ context.Context, s *stream.Stream) error {
	rec := startRec885{path: s.Path, zero: s.StartTimeNs == 0}
	if !rec.zero { // a request with no stamp reads the clock in HandleStream: it is fresh
		rec.age = time.Duration(mono.Now() - s.StartTimeNs)
	}
	h.recs <- rec
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (*startHandler885) RouteAsync(_, path string) bool { return strings.HasPrefix(path, "/async") }
func (*startHandler885) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = (*startHandler885)(nil)

func startStartTime885(t *testing.T, async bool) (*Engine, *startHandler885) {
	t.Helper()
	h := &startHandler885{recs: make(chan startRec885, 32)}
	return start543(t, async, h), h
}

// rec885 reads the next record the handler made, naming the path it expects.
func (h *startHandler885) rec(t *testing.T, path string) startRec885 {
	t.Helper()
	select {
	case r := <-h.recs:
		if r.path != path {
			t.Fatalf("apparatus: the handler ran for %q, want %q", r.path, path)
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("apparatus: the handler did not run for %q", path)
	}
	return startRec885{}
}

func readOKST885(t *testing.T, c net.Conn, br *bufio.Reader) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read a response: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Fatalf("response %d %q, want 200 ok", resp.StatusCode, b)
	}
}

func getST885(t *testing.T, c net.Conn, br *bufio.Reader, path string) {
	t.Helper()
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	readOKST885(t, c, br)
}

// TestEpollPromotedConnRequestStartIsFresh885: the requests of a promoted
// conn after an idle gap are stamped when they are read, not when the conn
// last read a request itself.
func TestEpollPromotedConnRequestStartIsFresh885(t *testing.T) {
	e, h := startStartTime885(t, true)
	c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	br := bufio.NewReader(c)

	// Inline first (the conn is the loop's), then the async route promotes it.
	getST885(t, c, br, "/sync")
	if r := h.rec(t, "/sync"); r.zero || r.age > staleLimit885 {
		t.Fatalf("apparatus: the first request, read inline, is %v old (stamp missing: %v)", r.age, r.zero)
	}
	getST885(t, c, br, "/async1")
	h.rec(t, "/async1")
	if got := e.Metrics().AsyncPromotedConns; got != 1 {
		t.Fatalf("apparatus: AsyncPromotedConns = %d, want 1", got)
	}

	// The dispatch goroutine serves what follows, an async route and a sync one.
	for _, path := range []string{"/async2", "/sync2"} {
		time.Sleep(idleGap885)
		getST885(t, c, br, path)
		if r := h.rec(t, path); r.age > staleLimit885 {
			t.Errorf("celeris#885: %s on a promoted conn, %v after the conn's last request, started %v before its handler ran "+
				"(the stamp is the conn's last inline read): the collector's duration, Context.StartTime and a detach's elapsed are inflated by the idle gap",
				path, idleGap885, r.age)
		}
	}
}

// TestEpollDirectBodyTailRequestStartIsFresh885: a request pipelined behind
// a body received straight into bodyBuf is stamped with the read that carried
// it, not with the one that carried the headers of the body's request, which
// may be as old as the upload is slow.
func TestEpollDirectBodyTailRequestStartIsFresh885(t *testing.T) {
	// Sync only: an AsyncHandlers loop reads into cs.buf alone (the
	// direct-body read is gated on !l.async), so it has no such tail.
	for _, async := range []bool{false} {
		name := "sync"
		t.Run(name, func(t *testing.T) {
			e, h := startStartTime885(t, async)
			c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			br := bufio.NewReader(c)
			base := e.Metrics().BytesRead
			sent := uint64(0)
			write := func(s string) {
				t.Helper()
				if _, err := io.WriteString(c, s); err != nil {
					t.Fatalf("write: %v", err)
				}
				sent += uint64(len(s))
			}

			// A large body first so bodyBuf has room, then the small POST
			// whose second half is received straight into it.
			big := strings.Repeat("x", 20000)
			write(postHead908("/p", len(big)) + big[:10000])
			waitServerRead908(t, e, base+sent)
			write(big[10000:])
			readOKST885(t, c, br)
			h.rec(t, "/p")
			small := strings.Repeat("y", 2000)
			write(postHead908("/q", len(small)) + small[:1000])
			waitServerRead908(t, e, base+sent)
			time.Sleep(idleGap885)
			write(small[1000:] + "GET /tail HTTP/1.1\r\nHost: x\r\n\r\n")
			readOKST885(t, c, br)
			readOKST885(t, c, br)
			h.rec(t, "/q")
			if r := h.rec(t, "/tail"); r.age > staleLimit885 {
				t.Errorf("celeris#885: the request pipelined behind a body that arrived %v after its headers started %v before its handler ran "+
					"(the stamp is the read of the headers of the body's request)", idleGap885, r.age)
			}
		})
	}
}

// TestEpollAdoptedCarryRequestStartIsFresh885 pins the replay of a
// transplant's carried bytes, which needs no stamp from the engine: the
// adopted conn's H1State is new (initProtocol, conn.NewH1State: NowNs 0), the
// handler then reads the clock itself. What follows on the conn, an async route
// after an idle gap, is stamped when the dispatch goroutine reads it. It passes
// on main for the replay and fails there for the request after the gap.
func TestEpollAdoptedCarryRequestStartIsFresh885(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e, h := startStartTime885(t, async)
			client, fd := adoptable543(t)
			if err := e.AdoptConn(fd, engine.Carryover{
				RemoteAddr: client.LocalAddr().String(),
				Buffered:   []byte("GET /carry HTTP/1.1\r\nHost: x\r\n\r\n"),
			}); err != nil {
				_ = unix.Close(fd)
				t.Fatalf("AdoptConn with a carry: %v", err)
			}
			br := bufio.NewReader(client)
			readOKST885(t, client, br)
			if r := h.rec(t, "/carry"); r.age > staleLimit885 {
				t.Errorf("the carried request started %v before its handler ran", r.age)
			}
			if !async {
				return
			}
			getST885(t, client, br, "/async1") // promotes the conn
			h.rec(t, "/async1")
			time.Sleep(idleGap885)
			getST885(t, client, br, "/async2")
			if r := h.rec(t, "/async2"); r.age > staleLimit885 {
				t.Errorf("celeris#885: /async2 on an adopted, promoted conn started %v before its handler ran", r.age)
			}
		})
	}
}
