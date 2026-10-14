//go:build linux

package celeris_test

import (
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
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/singleflight"
)

// celeris#944: an HTTP/2 client may split the Cookie header into one field
// per cookie (RFC 9113 §8.2.3), and the server must join the fields with
// "; " before anything reads one HTTP/1.1-style field. net/http's HTTP/2
// server does; the native engines (epoll, io_uring, adaptive) did not, so
// Context.Cookie saw the first field only, and singleflight's default key
// could coalesce two users who share their first cookie field.

// cookieProbe944 answers with what Context sees of the request's cookies.
func cookieProbe944(c *celeris.Context) error {
	sid, err := c.Cookie("sid")
	return c.String(200, "sid=%q err=%v header=%q", sid, err, c.Header("cookie"))
}

// h2Get944 sends one GET on a fresh h2c connection (prior knowledge) with the
// header fields extra after the pseudo-header fields, each its own field, in
// one HEADERS frame or (continuation) a HEADERS and a CONTINUATION, and
// returns the response's status and body.
func h2Get944(t *testing.T, addr, path string, extra [][2]string, continuation bool) (status string, body string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	fields := append([][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", path}}, extra...)
	for _, f := range fields {
		if err := enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]}); err != nil {
			t.Fatal(err)
		}
	}
	block := hb.Bytes()
	if continuation {
		mid := len(block) / 2
		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block[:mid], EndStream: true}); err != nil {
			t.Fatal(err)
		}
		if err := fr.WriteContinuation(1, true, block[mid:]); err != nil {
			t.Fatal(err)
		}
	} else if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block, EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	return readH2Response944(t, fr)
}

// readH2Response944 reads stream 1's response off fr.
func readH2Response944(t *testing.T, fr *http2.Framer) (status, body string) {
	t.Helper()
	var buf bytes.Buffer
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
		if f.Name == ":status" {
			status = f.Value
		}
	})
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("reading the response: %v (status %q, body so far %q)", err, status, buf.String())
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			if f.StreamID == 1 {
				_, _ = dec.Write(f.HeaderBlockFragment())
				if f.StreamEnded() {
					return status, buf.String()
				}
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				buf.Write(f.Data())
				if f.StreamEnded() {
					return status, buf.String()
				}
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("stream 1 was reset: %v", f.ErrCode)
		case *http2.GoAwayFrame:
			t.Fatalf("GOAWAY: %v %q", f.ErrCode, f.DebugData())
		}
	}
}

// TestH2SplitCookieFieldsAreJoined944 sends cookie fields split every way and
// checks that every engine hands the handler the same bytes net/http's HTTP/2
// server does (std is the reference, and is checked against strings.Join).
func TestH2SplitCookieFieldsAreJoined944(t *testing.T) {
	ck := func(v string) [2]string { return [2]string{"cookie", v} }
	type tcase struct {
		name         string
		fields       [][2]string
		continuation bool
		want         string // the joined Cookie header, as net/http builds it
	}
	cases := []tcase{
		{"one-field", [][2]string{ck("theme=dark; sid=alice")}, false, "theme=dark; sid=alice"},
		{"two-fields", [][2]string{ck("theme=dark"), ck("sid=alice")}, false, "theme=dark; sid=alice"},
		{"three-fields-continuation", [][2]string{ck("a=1"), ck("b=2"), ck("sid=carol")}, true, "a=1; b=2; sid=carol"},
		{"empty-field", [][2]string{ck("a=1"), ck(""), ck("sid=dave")}, false, "a=1; ; sid=dave"},
		{"other-field-between", [][2]string{ck("a=1"), {"x-other", "y"}, ck("sid=erin")}, false, "a=1; sid=erin"},
	}
	var std map[string]string
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) { s.GET("/c", cookieProbe944) })
			got := map[string]string{}
			for _, tc := range cases {
				status, body := h2Get944(t, addr, "/c", tc.fields, tc.continuation)
				if status != "200" {
					t.Fatalf("%s: status %q, body %q", tc.name, status, body)
				}
				got[tc.name] = body
				wantSid := ""
				if i := strings.LastIndex(tc.want, "sid="); i >= 0 {
					wantSid = tc.want[i+len("sid="):]
				}
				wantBody := fmt.Sprintf("sid=%q err=%v header=%q", wantSid, errIfEmpty944(wantSid), tc.want)
				if body != wantBody {
					t.Errorf("%s: the handler saw\n got %s\nwant %s", tc.name, body, wantBody)
				}
				if std != nil && body != std[tc.name] {
					t.Errorf("%s: not the bytes std (net/http) hands the handler:\n got %s\n std %s", tc.name, body, std[tc.name])
				}
			}
			if e.name == "std" {
				std = got
			}
		})
	}
}

func errIfEmpty944(sid string) string {
	if sid == "" {
		return "named cookie not present"
	}
	return "<nil>"
}

// TestH2TransportSplitCookieFields944 is the same through Go's own HTTP/2
// client (net/http over h2c with prior knowledge), which splits the Cookie
// header into one field per cookie as the RFC allows.
func TestH2TransportSplitCookieFields944(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) { s.GET("/c", cookieProbe944) })
			var protos http.Protocols
			protos.SetUnencryptedHTTP2(true)
			tr := &http.Transport{Protocols: &protos}
			defer tr.CloseIdleConnections()
			req, _ := http.NewRequest("GET", "http://"+addr+"/c", nil)
			req.Header.Set("Cookie", "theme=dark; sid=alice")
			resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.ProtoMajor != 2 {
				t.Fatalf("protocol HTTP/%d, want 2", resp.ProtoMajor)
			}
			want := `sid="alice" err=<nil> header="theme=dark; sid=alice"`
			if string(b) != want {
				t.Fatalf("the handler saw\n got %s\nwant %s", b, want)
			}
		})
	}
}

// TestH1CookieHeaderUnchanged944 checks that the join is HTTP/2 only: an
// HTTP/1.1 request's Cookie header reaches the handler as it always did.
func TestH1CookieHeaderUnchanged944(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) { s.GET("/c", cookieProbe944) })
			req, _ := http.NewRequest("GET", "http://"+addr+"/c", nil)
			req.Header.Set("Cookie", "theme=dark; sid=alice")
			resp, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.ProtoMajor != 1 {
				t.Fatalf("protocol HTTP/%d, want 1", resp.ProtoMajor)
			}
			want := `sid="alice" err=<nil> header="theme=dark; sid=alice"`
			if string(b) != want {
				t.Fatalf("the handler saw\n got %s\nwant %s", b, want)
			}
		})
	}
}

// TestH2SingleflightKeepsSplitCookieUsersApart944 is the cross-user case: two
// users share their first cookie field (a theme) and differ in the second (the
// session). With the first field alone in singleflight's default key, a
// request from one was handed the other's response while the leader ran.
// A pair with identical cookies must still coalesce, or the test proves
// nothing (the positive control).
func TestH2SingleflightKeepsSplitCookieUsersApart944(t *testing.T) {
	ck := func(v string) [2]string { return [2]string{"cookie", v} }
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			var calls atomic.Int64
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
				s.GET("/sf", singleflight.New(), func(c *celeris.Context) error {
					calls.Add(1)
					time.Sleep(300 * time.Millisecond)
					return c.String(200, "you=%s", c.Header("cookie"))
				}).Async()
			})
			// both sends two requests together, the second 60ms after the first.
			both := func(a, b [][2]string) (ra, rb string) {
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _, ra = h2Get944(t, addr, "/sf", a, false) }()
				time.Sleep(60 * time.Millisecond)
				go func() { defer wg.Done(); _, rb = h2Get944(t, addr, "/sf", b, false) }()
				wg.Wait()
				return
			}

			calls.Store(0)
			ra, rb := both([][2]string{ck("theme=dark"), ck("sid=alice")}, [][2]string{ck("theme=dark"), ck("sid=alice")})
			if calls.Load() != 1 || ra != rb {
				t.Fatalf("positive control: two identical requests ran the handler %d times (want 1: coalesced) and got %q / %q", calls.Load(), ra, rb)
			}

			calls.Store(0)
			ra, rb = both([][2]string{ck("theme=dark"), ck("sid=alice")}, [][2]string{ck("theme=dark"), ck("sid=bob")})
			if ra != "you=theme=dark; sid=alice" || rb != "you=theme=dark; sid=bob" || calls.Load() != 2 {
				t.Fatalf("two users with different session cookies: alice got %q, bob got %q, handler ran %d times; "+
					"want each their own response and 2 runs", ra, rb, calls.Load())
			}
		})
	}
}
