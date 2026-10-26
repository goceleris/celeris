//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/singleflight"
)

// celeris#889 item 16, on every engine: a request that sends several Cookie
// lines reaches the handler as one cookie field joined with "; " (HTTP/2
// does the same at HPACK decode, celeris#944), over plain HTTP/1.1 and over
// the RFC 7540 §3.2 h2c Upgrade request, which is replayed as HTTP/2 stream 1
// with its HTTP/1 lines. Without the join, Context.Cookie read every line but
// Header("cookie"), the singleflight key, the cache key (VaryHeaders) and the
// Vary check of singleflight read the first, so two clients with the same
// first line and different later lines were handed one another's response
// (found in review r1 of the PR that made Cookie read every line).
//
// /hdr is deterministic. /who, /vary and /adapt overlap two clients (a leak
// shows only if the requests overlapped); /cached is sequential.

func cookieReq889(addr, path, sid string, upgrade bool) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if !upgrade {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nCookie: lang=en\r\nCookie: sid=%s\r\nConnection: close\r\n\r\n", path, sid)
		b, _ := io.ReadAll(conn)
		return string(b), nil
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAAP__\r\nCookie: lang=en\r\nCookie: sid=%s\r\n\r\n", path, sid)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.Contains(status, "101") {
		rest, _ := io.ReadAll(br)
		return status + string(rest), nil // not upgraded: the plain HTTP/1.1 answer
	}
	for { // the rest of the 101 head
		l, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		if l == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte(http2.ClientPreface))
	fr := http2.NewFramer(conn, br)
	if err := fr.WriteSettings(); err != nil {
		return "", err
	}
	// Stream 1's response, to its END_STREAM.
	var body bytes.Buffer
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return body.String(), err
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			if f.StreamID == 1 && f.StreamEnded() {
				return body.String(), nil
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				body.Write(f.Data())
				if f.StreamEnded() {
					return body.String(), nil
				}
			}
		case *http2.RSTStreamFrame:
			return body.String(), fmt.Errorf("stream %d reset: %v", f.StreamID, f.ErrCode)
		case *http2.GoAwayFrame:
			return body.String(), fmt.Errorf("GOAWAY: %v", f.ErrCode)
		}
	}
}

func TestSeveralCookieLinesAreOneFieldOnEveryEngine889(t *testing.T) {
	for _, e := range engines761 {
		for _, mode := range []string{"h1", "h2c-upgrade"} {
			for _, path := range []string{"/hdr", "/who", "/adapt", "/vary", "/cached"} {
				t.Run(e.name+"/"+mode+path, func(t *testing.T) {
					var runs atomic.Int32
					addr := startServer421(t, e.eng, func(s *celeris.Server) {
						who := func(c *celeris.Context) error {
							runs.Add(1)
							v, err := c.Cookie("sid")
							if err != nil {
								v = "none"
							}
							c.SetHeader("vary", "Cookie")
							time.Sleep(300 * time.Millisecond)
							return c.String(200, "SID<%s>", v)
						}
						s.GET("/hdr", func(c *celeris.Context) error {
							n := 0
							for _, h := range c.RequestHeaders() {
								if h[0] == "cookie" {
									n++
								}
							}
							v, _ := c.Cookie("sid")
							return c.String(200, "FIELDS<%d> HDR<%s> SID<%s>", n, c.Header("cookie"), v)
						})
						s.GET("/who", singleflight.New(), who)
						s.GET("/vary", singleflight.New(singleflight.Config{KeyFunc: func(c *celeris.Context) string { return c.Path() }}), who)
						s.GET("/cached", cache.New(cache.Config{VaryHeaders: []string{"Cookie"}}), who)
						s.GET("/adapt", singleflight.New(), celeris.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
							runs.Add(1)
							v := "none"
							if ck, err := r.Cookie("sid"); err == nil {
								v = ck.Value
							}
							time.Sleep(300 * time.Millisecond)
							w.Header().Set("Content-Type", "text/plain")
							_, _ = io.WriteString(w, "SID<"+v+">")
						}))
					})
					upgrade := mode == "h2c-upgrade"
					if path == "/hdr" {
						out, err := cookieReq889(addr, path, "ALICE", upgrade)
						if err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(out, "FIELDS<1> HDR<lang=en; sid=ALICE> SID<ALICE>") {
							t.Fatalf("the handler saw %q, want one cookie field \"lang=en; sid=ALICE\"", out)
						}
						return
					}
					var wg sync.WaitGroup
					var mu sync.Mutex
					got := map[string]string{}
					for _, sid := range []string{"ALICE", "MALLORY"} {
						if path == "/cached" {
							wg.Wait() // sequential: the second request is a cache lookup
						}
						wg.Add(1)
						go func() {
							defer wg.Done()
							out, err := cookieReq889(addr, path, sid, upgrade)
							if err != nil {
								t.Error(err)
							}
							mu.Lock()
							got[sid] = out
							mu.Unlock()
						}()
					}
					wg.Wait()
					for sid, out := range got {
						if !strings.Contains(out, "SID<"+sid+">") {
							t.Errorf("client %s was answered %q (handler ran %d times): another client's response", sid, out[:min(len(out), 300)], runs.Load())
						}
					}
				})
			}
		}
	}
}
