package std

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"

	"golang.org/x/net/http2"
)

// These tests pin celeris#889 (item 14), the std engine's side of
// Config.EnableH2Upgrade: the same answers as epoll, io_uring and adaptive for
// every Protocol, with the *bool as celeris.Config resolves it
// (toResourceConfig: nil is true on Auto and false on H2C and HTTP1).
//
//	Protocol  prior-knowledge h2c  Upgrade: h2c
//	Auto      yes                  unless &false
//	H2C       yes                  only &true
//	HTTP1     no                   only &true
//
// Before #889 std upgraded on H2C for nil and &false as well, and never on
// HTTP1.

// resolve889 is celeris.Config.EnableH2Upgrade as toResourceConfig turns it
// into the resource.Config the engine gets: nil is protocol dependent, a
// value is final.
func resolve889(proto engine.Protocol, enable *bool) func(*resource.Config) {
	return func(c *resource.Config) {
		c.Protocol = proto
		switch {
		case enable != nil:
			c.SetH2Upgrade(*enable)
		default:
			c.SetH2Upgrade(proto == engine.Auto)
		}
	}
}

func bool889(v bool) *bool { return &v }

// protocols889 are the three protocols, with the upgrade and prior-knowledge
// answers the table above gives for each EnableH2Upgrade value.
var protocols889 = []struct {
	proto          engine.Protocol
	priorKnowledge bool
	upgrades       map[string]bool // by "nil", "false", "true"
}{
	{engine.Auto, true, map[string]bool{"nil": true, "false": false, "true": true}},
	{engine.H2C, true, map[string]bool{"nil": false, "false": false, "true": true}},
	{engine.HTTP1, false, map[string]bool{"nil": false, "false": false, "true": true}},
}

var enables889 = []struct {
	name string
	v    *bool
}{{"nil", nil}, {"false", bool889(false)}, {"true", bool889(true)}}

const upgradeWire889 = "GET /upgrade HTTP/1.1\r\nHost: std.test\r\n" +
	"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: \r\n\r\n"

const plainWire889 = "GET /plain HTTP/1.1\r\nHost: std.test\r\n\r\n"

// TestEnableH2UpgradeMatrix889: an Upgrade: h2c request is answered 101 and
// served as stream 1 exactly where the table says; everywhere else it is
// served as plain HTTP/1.1 (200), the connection stays HTTP/1.1 (a second
// request on it is answered the same way), and the handler sees each request
// once.
func TestEnableH2UpgradeMatrix889(t *testing.T) {
	for _, p := range protocols889 {
		for _, en := range enables889 {
			t.Run(p.proto.String()+"/"+en.name, func(t *testing.T) {
				h := newPathHandler()
				_, addr := startH2CEngineWith(t, h, nil, resolve889(p.proto, en.v))
				if want := p.upgrades[en.name]; want {
					r := dialUpgradeRawH2(t, addr, "/upgrade")
					ev, _, ok := r.waitFor(5*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders && ev.stream == 1 })
					if !ok || ev.status != "200" {
						t.Fatalf("upgraded connection: ok=%v event=%+v, want a 200 HEADERS frame on stream 1", ok, ev)
					}
					if got := h.snapshot(); len(got) != 1 || got[0] != "/upgrade" {
						t.Fatalf("handler saw %v, want exactly [/upgrade]", got)
					}
					return
				}
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				br := bufio.NewReader(c)
				for i, wire := range []string{upgradeWire889, plainWire889} {
					if _, err := io.WriteString(c, wire); err != nil {
						t.Fatalf("write %d: %v", i, err)
					}
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						peek, _ := br.Peek(br.Buffered())
						t.Fatalf("response %d is not HTTP/1.1: %v (next bytes %q)", i+1, err, peek)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 || resp.ProtoMajor != 1 {
						t.Fatalf("response %d: %s %d, want HTTP/1.1 200 (no 101)", i+1, resp.Proto, resp.StatusCode)
					}
				}
				if got := h.snapshot(); len(got) != 2 || got[0] != "/upgrade" || got[1] != "/plain" {
					t.Fatalf("handler saw %v, want [/upgrade /plain]: each request served once, as HTTP/1.1", got)
				}
			})
		}
	}
}

// firstReply889 sends wire and returns what the server sent first: up to the
// first CRLF or 9 bytes (an HTTP/2 frame header), or whatever came before the
// connection ended or 3 s passed.
func firstReply889(t *testing.T, addr, wire string) []byte {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, wire); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	buf := make([]byte, 512)
	for len(got) < 9 && !bytes.Contains(got, []byte("\r\n")) {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	return got
}

// isSettingsFrame889 reports whether b starts with an HTTP/2 SETTINGS frame
// header (type 0x04, stream 0): what a server that took the connection for
// HTTP/2 sends first.
func isSettingsFrame889(b []byte) bool {
	return len(b) >= 9 && b[3] == 0x04 && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// TestPriorKnowledgeH2COnlyWhereTheProtocolHasIt889: a connection that opens
// with the HTTP/2 preface is served as HTTP/2 on Auto and H2C, whatever
// EnableH2Upgrade says, and is NOT on HTTP1, EnableH2Upgrade &true included:
// there the h2c front end exists for the upgrade alone.
func TestPriorKnowledgeH2COnlyWhereTheProtocolHasIt889(t *testing.T) {
	var wire bytes.Buffer
	wire.WriteString(http2.ClientPreface)
	if err := http2.NewFramer(&wire, nil).WriteSettings(); err != nil {
		t.Fatal(err)
	}
	for _, p := range protocols889 {
		for _, en := range enables889 {
			t.Run(p.proto.String()+"/"+en.name, func(t *testing.T) {
				_, addr := startH2CEngineWith(t, newPathHandler(), nil, resolve889(p.proto, en.v))
				got := firstReply889(t, addr, wire.String())
				if p.priorKnowledge {
					if !isSettingsFrame889(got) {
						t.Fatalf("HTTP/2 preface answered %q, want an HTTP/2 SETTINGS frame", got)
					}
					return
				}
				if isSettingsFrame889(got) {
					t.Fatalf("Protocol HTTP1 took the HTTP/2 preface for prior-knowledge h2c and answered %q", got)
				}
				if len(got) != 0 && !strings.HasPrefix(string(got), "HTTP/1.1 ") {
					t.Fatalf("HTTP/2 preface on Protocol HTTP1 answered %q, want an HTTP/1.1 status line or a closed connection", got)
				}
				t.Logf("Protocol HTTP1, EnableH2Upgrade %s: HTTP/2 preface answered %q", en.name, got)
			})
		}
	}
}

// BenchmarkFrontEndServeHTTP889 measures what #889 puts in front of the bridge
// for an ordinary HTTP/1.1 request: the engine's own http.Handler
// (e.server.Handler), not Bridge.ServeHTTP alone (BenchmarkBridgeServeHTTP
// builds its Bridge by hand and never reaches the front end). On HTTP1 with
// the upgrade off it is the bridge itself, the control; on HTTP1 with &true
// (new) and on H2C and Auto it is h2cHandler, whose per-request cost is the
// prior-knowledge test and, when the upgrade is on, the two header lookups of
// isH2CUpgrade. The upgrade itself happens once per connection.
func BenchmarkFrontEndServeHTTP889(b *testing.B) {
	for _, tc := range []struct {
		name   string
		proto  engine.Protocol
		enable bool
	}{
		{"HTTP1Off", engine.HTTP1, false},
		{"HTTP1On", engine.HTTP1, true},
		{"H2COff", engine.H2C, false},
		{"H2COn", engine.H2C, true},
		{"AutoOff", engine.Auto, false},
		{"AutoOn", engine.Auto, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cfg := resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: tc.proto}
			cfg.SetH2Upgrade(tc.enable)
			e, err := New(cfg, okStreamHandler{})
			if err != nil {
				b.Fatal(err)
			}
			h := e.server.Handler
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := &discardResponseWriter{h: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				h.ServeHTTP(w, req)
			}
		})
	}
}

// BenchmarkIsH2CUpgrade889 is the one per-request addition #889 makes where
// the upgrade is on (HTTP1 with &true), alone: the upgrade test on the headers
// of an ordinary request, with none and with the usual few.
func BenchmarkIsH2CUpgrade889(b *testing.B) {
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"NoHeaders", http.Header{}},
		{"Usual", http.Header{"Accept": {"*/*"}, "User-Agent": {"x"}, "Host": {"h"}, "Connection": {"keep-alive"}, "Accept-Encoding": {"gzip"}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if isH2CUpgrade(tc.h) {
					b.Fatal("not an upgrade")
				}
			}
		})
	}
}

// TestIsH2CUpgradeTakesSoleToken889: the Upgrade header is an upgrade to h2c
// only when every line of it is exactly h2c, whatever the case or the
// spacing, and Connection lists both Upgrade and HTTP2-Settings (the native
// parser needs both: internal/protocol/h1 detectH2CHeader). A list that offers other
// protocols as well is not one (SECURITY.md: it would let a client that asked
// a proxy or server for a choice, h2c or websocket, get h2c).
func TestIsH2CUpgradeTakesSoleToken889(t *testing.T) {
	conn := []string{"Upgrade, HTTP2-Settings"}
	for _, tc := range []struct {
		name string
		h    http.Header
		want bool
	}{
		{"h2c", http.Header{"Upgrade": {"h2c"}, "Connection": conn}, true},
		{"H2C padded", http.Header{"Upgrade": {"  H2C "}, "Connection": conn}, true},
		{"h2c twice", http.Header{"Upgrade": {"h2c", "h2c"}, "Connection": conn}, true},
		{"websocket then h2c", http.Header{"Upgrade": {"websocket, h2c"}, "Connection": conn}, false},
		{"h2c then foo", http.Header{"Upgrade": {"h2c, foo"}, "Connection": conn}, false},
		{"two lines", http.Header{"Upgrade": {"websocket", "h2c"}, "Connection": conn}, false},
		{"two lines, h2c first", http.Header{"Upgrade": {"h2c", "websocket"}, "Connection": conn}, false},
		{"other protocol", http.Header{"Upgrade": {"websocket"}, "Connection": conn}, false},
		{"no Upgrade", http.Header{"Connection": conn}, false},
		{"no HTTP2-Settings token", http.Header{"Upgrade": {"h2c"}, "Connection": {"Upgrade"}}, false},
		{"no Upgrade token", http.Header{"Upgrade": {"h2c"}, "Connection": {"HTTP2-Settings"}}, false},
		{"no Connection", http.Header{"Upgrade": {"h2c"}}, false},
		{"tokens on two Connection lines", http.Header{"Upgrade": {"h2c"}, "Connection": {"HTTP2-Settings", "upgrade"}}, true},
		{"tokens in other case", http.Header{"Upgrade": {"h2c"}, "Connection": {"keep-alive, UPGRADE, http2-settings"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isH2CUpgrade(tc.h); got != tc.want {
				t.Fatalf("isH2CUpgrade(%v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}

// TestMultiTokenUpgradeIsNotUpgraded889: on the wire, on every protocol that
// upgrades, a request that offers h2c among other protocols is served as
// HTTP/1.1 (200), not answered 101.
func TestMultiTokenUpgradeIsNotUpgraded889(t *testing.T) {
	const wire = "GET /upgrade HTTP/1.1\r\nHost: std.test\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: websocket, h2c\r\nHTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"
	for _, p := range protocols889 {
		t.Run(p.proto.String(), func(t *testing.T) {
			_, addr := startH2CEngineWith(t, newPathHandler(), nil, resolve889(p.proto, bool889(true)))
			got := firstReply889(t, addr, wire)
			if !strings.HasPrefix(string(got), "HTTP/1.1 200") {
				t.Fatalf("Upgrade: websocket, h2c on Protocol %v with the upgrade on answered %q, want HTTP/1.1 200", p.proto, got)
			}
		})
	}
}

// TestConnectionWithoutUpgradeTokenIsNotUpgraded889: on the wire, on every
// protocol that upgrades, a request with Upgrade: h2c and HTTP2-Settings but a
// Connection that lists only HTTP2-Settings is served as HTTP/1.1 (200), as
// the native engines do, not answered 101: a proxy that does not see
// "Connection: Upgrade" forwards it as an ordinary request.
func TestConnectionWithoutUpgradeTokenIsNotUpgraded889(t *testing.T) {
	const wire = "GET /upgrade HTTP/1.1\r\nHost: std.test\r\n" +
		"Connection: HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"
	for _, p := range protocols889 {
		t.Run(p.proto.String(), func(t *testing.T) {
			_, addr := startH2CEngineWith(t, newPathHandler(), nil, resolve889(p.proto, bool889(true)))
			got := firstReply889(t, addr, wire)
			if !strings.HasPrefix(string(got), "HTTP/1.1 200") {
				t.Fatalf("Connection: HTTP2-Settings alone on Protocol %v with the upgrade on answered %q, want HTTP/1.1 200", p.proto, got)
			}
		})
	}
}
