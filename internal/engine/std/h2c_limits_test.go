package std

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// sendHeaders writes a HEADERS frame for a request whose pseudo-headers are
// the usual ones plus extra (name, value pairs), on a stream the caller picks.
func (r *rawH2) sendHeaders(id uint32, method, path string, endStream bool, extra ...[2]string) {
	r.t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	fields := [][2]string{{":method", method}, {":scheme", "http"}, {":authority", "std.test"}, {":path", path}}
	for _, kv := range append(fields, extra...) {
		_ = enc.WriteField(hpack.HeaderField{Name: kv[0], Value: kv[1]})
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: buf.Bytes(), EndStream: endStream, EndHeaders: true,
	}); err != nil {
		r.t.Logf("write HEADERS on stream %d: %v", id, err)
	}
}

// endsStream reports whether ev says the server is done with the stream or the
// connection: a response, a reset, GOAWAY, or the end of the connection.
func endsStream(ev h2Event) bool {
	return ev.endsConn || ev.typ == http2.FrameHeaders || ev.typ == http2.FrameRSTStream || ev.typ == http2.FrameGoAway
}

// The tests below pin that an h2c connection is served under the engine's own
// limits (Config.MaxHeaderBytes, ReadTimeout, IdleTimeout), not under the
// zero http.Server that x/net's HTTP/2 server falls back to when it is handed
// no BaseConfig. On go1.27 the net/http wrapper reads them from the server the
// connection is served through (e.server); with -tags http2legacy x/net's own
// server reads them from BaseConfig, which h2cBaseConfig supplies there.

// TestH2CConnKeepsMaxHeaderBytes: a 12 KiB header on a server configured with
// MaxHeaderBytes=4096 must not reach the handler and must not be answered 200.
func TestH2CConnKeepsMaxHeaderBytes(t *testing.T) {
	h := newPathHandler()
	_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) { c.MaxHeaderBytes = 4096 })

	// The server answers a small request on the same configuration, so the
	// refusal below is the limit, not a server that answers nothing.
	ok := dialRawH2(t, addr, false)
	ok.sendHeaders(1, "GET", "/small", true)
	if ev, seen, found := ok.waitFor(5*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders }); !found || ev.status != "200" {
		t.Fatalf("a small request was not answered 200 (found=%v, events %+v)", found, seen)
	}

	big := dialRawH2(t, addr, false)
	big.sendHeaders(1, "GET", "/big", true, [2]string{"x-big", strings.Repeat("a", 12<<10)})
	ev, seen, _ := big.waitFor(5*time.Second, endsStream)
	for _, e := range seen {
		if e.typ == http2.FrameHeaders && e.status == "200" {
			t.Fatalf("a 12 KiB header was answered 200 on a server with MaxHeaderBytes=4096 (events %+v)", seen)
		}
	}
	if !endsStream(ev) {
		t.Fatalf("the server did not refuse a 12 KiB header (MaxHeaderBytes=4096) within 5s (events %+v)", seen)
	}
	for _, p := range h.snapshot() {
		if p == "/big" {
			t.Fatal("a 12 KiB header reached the handler on a server with MaxHeaderBytes=4096")
		}
	}
}

// TestH2CConnKeepsReadTimeout: a stream that announces a body and never sends
// it must be ended by Config.ReadTimeout, not held for ever.
func TestH2CConnKeepsReadTimeout(t *testing.T) {
	h := newPathHandler()
	_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) {
		c.ReadTimeout = 300 * time.Millisecond
		c.ReadHeaderTimeout = 300 * time.Millisecond
	})
	c := dialRawH2(t, addr, false)
	start := time.Now()
	c.sendHeaders(1, "POST", "/slow", false, [2]string{"content-length", "10"})
	_, seen, found := c.waitFor(3*time.Second, endsStream)
	if !found {
		t.Fatalf("ReadTimeout=300ms did not end a stream whose body never arrived within %v (events %+v)", time.Since(start).Round(time.Millisecond), seen)
	}
}

// TestH2CConnGetsGoAwayAfterIdleTimeout: Config.IdleTimeout applies to an h2c
// connection: once idle for that long the server sends GOAWAY (NO_ERROR).
// Before celeris#878's front end the connection was served on a one-off server
// with no IdleTimeout, so an idle h2c connection was never closed.
func TestH2CConnGetsGoAwayAfterIdleTimeout(t *testing.T) {
	h := newPathHandler()
	_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) { c.IdleTimeout = 300 * time.Millisecond })
	c := dialRawH2(t, addr, false)
	c.sendHeaders(1, "GET", "/once", true)
	if ev, seen, found := c.waitFor(5*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders }); !found || ev.status != "200" {
		t.Fatalf("the one request was not answered 200 (found=%v, events %+v)", found, seen)
	}
	start := time.Now()
	ev, seen, found := c.waitFor(3*time.Second, isGoAway)
	if !found {
		t.Fatalf("no GOAWAY %v after the connection went idle with IdleTimeout=300ms (events %+v)", time.Since(start).Round(time.Millisecond), seen)
	}
	if ev.code != http2.ErrCodeNo {
		t.Errorf("idle GOAWAY carried %v, want NO_ERROR", ev.code)
	}
}
