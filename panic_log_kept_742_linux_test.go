//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// mw742KeepHandler keeps every record, cloned as slog requires, and formats
// nothing until asked: what an asynchronous or batching handler does.
type mw742KeepHandler struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h mw742KeepHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h mw742KeepHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.recs = append(*h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h mw742KeepHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h mw742KeepHandler) WithGroup(string) slog.Handler      { return h }

// TestHandlerPanicLogSurvivesNextRequest pins the server's own panic log
// (handler.go, handlePanic), a site of the celeris#732 class that #742 does
// not list.
//
// When a handler panics and no recovery middleware catches it, the server
// logs the method and path to Config.Logger, any *slog.Logger. slog lets a
// Handler keep a Record after Handle returns by calling Record.Clone, which
// shares the strings; asynchronous and batching handlers do. On epoll and
// io_uring (and Adaptive, which runs them) the method (for one the H1 parser
// does not intern) and the path are views of the connection's receive
// buffer, which the engine reuses for the connection's next request. Three
// requests with the same layout and different values go over one keep-alive
// connection; every kept record must still read its own request.
func TestHandlerPanicLogSurvivesNextRequest(t *testing.T) {
	const msg = "handler panic recovered"
	methods := []string{"TRACE", "PURGE", "MKCOL"}
	for _, a := range mw742Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			var mu sync.Mutex
			var recs []slog.Record
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{
					Engine:        a.engine,
					AsyncHandlers: a.async,
					Logger:        slog.New(mw742KeepHandler{mu: &mu, recs: &recs}),
				})
				for _, m := range methods {
					srv.Handle(m, "/p/:id", func(*celeris.Context) error { panic("boom") })
				}
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []map[string]string
			for i, m := range methods {
				v := strings.Repeat(string(rune('a'+i)), 4)
				mw742RoundTrip(t, conn, br, m+" /p/"+v+" HTTP/1.1\r\nHost: h\r\n\r\n", 500)
				want = append(want, map[string]string{"method": m, "path": "/p/" + v})
			}

			// The engines log to the same Logger; keep only the panic records.
			var kept []slog.Record
			for i := 0; i < 200; i++ {
				kept = kept[:0]
				mu.Lock()
				for _, r := range recs {
					if r.Message == msg {
						kept = append(kept, r)
					}
				}
				mu.Unlock()
				if len(kept) >= len(methods) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(kept) != len(methods) {
				t.Fatalf("got %d %q records, want %d", len(kept), msg, len(methods))
			}
			sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
			var wrong []string
			for i, r := range kept {
				got := map[string]string{}
				r.Attrs(func(a slog.Attr) bool {
					if a.Value.Kind() == slog.KindString {
						got[a.Key] = a.Value.String()
					}
					return true
				})
				for _, k := range []string{"method", "path"} {
					if got[k] != want[i][k] {
						wrong = append(wrong, fmt.Sprintf("record %d %s: %q (want %q)", i+1, k, got[k], want[i][k]))
					}
				}
			}
			t.Logf("MW742PANICLOG arm=%s records=%d wrong=%d", a.name, len(kept), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("a kept panic record reads other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}

// mw742Arms is keptArms plus Adaptive with sync and async handlers.
func mw742Arms(t *testing.T) []keptArm {
	t.Helper()
	return append(keptArms(t), keptArm{"adaptive", celeris.Adaptive, false}, keptArm{"adaptive-async", celeris.Adaptive, true})
}

// mw742RoundTrip writes one request and reads its whole response; it fails
// the test unless the status is want.
func mw742RoundTrip(t *testing.T, conn net.Conn, br *bufio.Reader, req string, want int) {
	t.Helper()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	n := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if _, err := br.Discard(n); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if f := strings.Fields(status); len(f) < 2 || f[1] != strconv.Itoa(want) {
		t.Fatalf("status %q for %q, want %d", strings.TrimSpace(status), strings.SplitN(req, "\r\n", 2)[0], want)
	}
}
