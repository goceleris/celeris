package std

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"

	"golang.org/x/net/http2"
)

// celeris#976: the h2c front end read the body of an RFC 7540 3.2 upgrade
// request whole (io.ReadAll) before it hijacked the connection, so Config.
// MaxRequestBodySize, which Bridge applies afterwards, never bounded it: a
// client that asked for the upgrade with a 32 MiB body made the server
// buffer all of it and answer 101. The native engines refuse such a request
// with 413 after about a MiB. Like the #964 tests, these run on every
// platform: std is the only engine on macOS and Windows.

// upgradeBodyOutcome976 is what a client saw when it sent an Upgrade: h2c
// POST with a body of n bytes.
type upgradeBodyOutcome976 struct {
	first      string // the status line of the first reply ("" if none)
	sent       int64  // body bytes the client got onto the connection
	allocBytes uint64 // server+client TotalAlloc growth in this process
}

func sendUpgradeBody976(t *testing.T, addr string, n int64) upgradeBodyOutcome976 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	chunk := make([]byte, 1<<20) // allocated before the baseline below
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	head := fmt.Sprintf("POST /h HTTP/1.1\r\nHost: std.test\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\nContent-Length: %d\r\n\r\n", n)
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatalf("write head: %v", err)
	}
	var sent atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for sent.Load() < n {
			k := int64(len(chunk))
			if rest := n - sent.Load(); rest < k {
				k = rest
			}
			w, err := c.Write(chunk[:k])
			sent.Add(int64(w))
			if err != nil {
				return
			}
		}
	}()
	line, _ := bufio.NewReader(c).ReadString('\n')
	_ = c.Close() // ends the writer if the server is still reading
	<-writerDone
	runtime.ReadMemStats(&m1)
	return upgradeBodyOutcome976{first: strings.TrimSpace(line), sent: sent.Load(), allocBytes: m1.TotalAlloc - m0.TotalAlloc}
}

// TestAutoUpgradeBodyBoundedByMaxRequestBodySize976: a 32 MiB body on an
// Upgrade: h2c request, MaxRequestBodySize 4096, Protocol Auto and the
// default (upgrade on): refused with 413 after a bounded read and a bounded
// allocation. Before the fix: 101, all 33554432 bytes read, +74 MiB.
func TestAutoUpgradeBodyBoundedByMaxRequestBodySize976(t *testing.T) {
	const n = 32 << 20
	_, addr := startAutoEngine(t, func(c *resource.Config) { c.MaxRequestBodySize = 4096 })
	o := sendUpgradeBody976(t, addr, n)
	t.Logf("MEASURE std first=%q sent=%d TotalAllocDeltaMiB=%d", o.first, o.sent, o.allocBytes>>20)
	if !strings.HasPrefix(o.first, "HTTP/1.1 413") {
		t.Fatalf("a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", n, o.first)
	}
	if o.sent > n/2 {
		t.Fatalf("the server took %d of %d body bytes before refusing, want at most %d (a bounded read)", o.sent, n, n/2)
	}
	if o.allocBytes > n/2 {
		t.Fatalf("serving the refused request allocated %d MiB, want at most %d MiB", o.allocBytes>>20, (n/2)>>20)
	}
}

// TestH2CUpgradeBodyBoundedByMaxRequestBodySize976 is the same on Protocol
// H2C with the upgrade turned on (EnableH2Upgrade &true, which H2C needs for
// the upgrade since celeris#889).
func TestH2CUpgradeBodyBoundedByMaxRequestBodySize976(t *testing.T) {
	const n = 32 << 20
	e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.H2C
		c.SetH2Upgrade(true)
		c.MaxRequestBodySize = 4096
	})
	o := sendUpgradeBody976(t, addr, n)
	t.Logf("MEASURE std(h2c) first=%q sent=%d TotalAllocDeltaMiB=%d", o.first, o.sent, o.allocBytes>>20)
	if !strings.HasPrefix(o.first, "HTTP/1.1 413") {
		t.Fatalf("a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", n, o.first)
	}
	if o.sent > n/2 {
		t.Fatalf("the server took %d of %d body bytes before refusing, want at most %d", o.sent, n, n/2)
	}
	// Bridge counts a refused body as a request-body error; so does this.
	deadline := time.Now().Add(5 * time.Second)
	for e.Metrics().ErrorRequestBody == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.Metrics().ErrorRequestBody; got != 1 {
		t.Fatalf("ErrorRequestBody = %d after one refused upgrade body, want 1", got)
	}
}

// TestUpgradeBodyWithinLimitStillUpgrades976 is the control for the bound: a
// body within MaxRequestBodySize is still answered 101 (the connection is
// taken over), and an unlimited config (-1) takes any size.
// TestUpgradeBodyReachesStreamOneIntact976 checks the body itself.
func TestUpgradeBodyWithinLimitStillUpgrades976(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		body  int64
	}{
		{"at-the-limit", 4096, 4096},
		{"under-the-limit", 4096, 100},
		{"unlimited", -1, 1 << 20},
		{"no-body", 4096, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := startAutoEngine(t, func(c *resource.Config) { c.MaxRequestBodySize = tc.limit })
			o := sendUpgradeBody976(t, addr, tc.body)
			if !strings.HasPrefix(o.first, "HTTP/1.1 101") {
				t.Fatalf("a %d byte Upgrade: h2c body under limit %d was answered %q, want 101", tc.body, tc.limit, o.first)
			}
		})
	}
}

// TestRefusedUpgradeBodyCountsLikeBridge976: a request refused for its body
// is a request and a request-body error, whether the upgrade refuses it
// (before the hijack) or Bridge does (the upgrade is off, or the request is
// plain HTTP/1.1): the engine's metrics must not depend on which.
func TestRefusedUpgradeBodyCountsLikeBridge976(t *testing.T) {
	const n = 64 << 10
	for _, tc := range []struct {
		name  string
		proto engine.Protocol
		noUp  bool
	}{
		{"h2c-upgrade", engine.H2C, false},
		{"h2c-upgrade-off-bridge", engine.H2C, true},
		{"auto-upgrade", engine.Auto, false},
		{"auto-upgrade-off-bridge", engine.Auto, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
				c.Protocol = tc.proto
				c.MaxRequestBodySize = 4096
				c.SetH2Upgrade(!tc.noUp)
			})
			o := sendUpgradeBody976(t, addr, n)
			if !strings.HasPrefix(o.first, "HTTP/1.1 413") {
				t.Fatalf("a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", n, o.first)
			}
			deadline := time.Now().Add(5 * time.Second)
			for e.Metrics().ErrorRequestBody == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			m := e.Metrics()
			if m.ErrorRequestBody != 1 || m.RequestCount != 1 {
				t.Fatalf("after one refused request: ErrorRequestBody = %d, RequestCount = %d, want 1 and 1", m.ErrorRequestBody, m.RequestCount)
			}
		})
	}
}

// bodyRecorder976 hands the body of every request to a channel.
type bodyRecorder976 struct{ bodies chan []byte }

func (h *bodyRecorder976) HandleStream(_ context.Context, s *stream.Stream) error {
	h.bodies <- bytes.Clone(s.GetData())
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
}

// TestUpgradeBodyReachesStreamOneIntact976: a body within the limit is
// served as stream 1 after the 101, byte for byte (the bounded read must not
// truncate it, and the handler must still be given it).
func TestUpgradeBodyReachesStreamOneIntact976(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		body  int
	}{
		{"at-the-limit", 4096, 4096},
		{"under-the-limit", 4096, 100},
		{"unlimited", -1, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &bodyRecorder976{bodies: make(chan []byte, 4)}
			_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.MaxRequestBodySize = tc.limit
			})
			want := make([]byte, tc.body)
			for i := range want {
				want[i] = byte(i % 251)
			}
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			head := fmt.Sprintf("POST /h HTTP/1.1\r\nHost: std.test\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
				"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\nContent-Length: %d\r\n\r\n", tc.body)
			if _, err := c.Write(append([]byte(head), want...)); err != nil {
				t.Fatalf("write: %v", err)
			}
			br := bufio.NewReader(c)
			for { // the 101 response head
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("read the 101: %v", err)
				}
				if line == "\r\n" {
					break
				}
			}
			// The client preface and an empty SETTINGS frame start HTTP/2.
			if _, err := c.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00")); err != nil {
				t.Fatalf("write preface: %v", err)
			}
			select {
			case got := <-h.bodies:
				if !bytes.Equal(got, want) {
					t.Fatalf("stream 1 got %d body bytes (equal=false), want the %d sent", len(got), len(want))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the upgrade request was not served as stream 1")
			}
		})
	}
}

// celeris#889 item 25 (from the #977 round-2 review): h2cUpgrade and
// Bridge.ServeHTTP read up to MaxRequestBodySize+1 bytes before they looked at
// a declared Content-Length, so a request declaring a body over the limit sat
// in the read until the client sent the bytes, or until ReadTimeout, where it
// can be refused at once. The tests below send the head of such a request and
// withhold the body: the 413 must come without waiting for it.

// replyWithheld976 writes head to a new connection, sends no body, and returns
// the first line of the reply, or "" if none came within wait.
func replyWithheld976(t *testing.T, addr, head string, wait time.Duration) (first string, took time.Duration) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	_ = c.SetDeadline(start.Add(wait))
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatalf("write head: %v", err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), time.Since(start)
}

const upgradeHead976 = "POST /h HTTP/1.1\r\nHost: std.test\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
	"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n"

func waitRequestBodyError976(t *testing.T, e *Engine, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.Metrics().ErrorRequestBody < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.Metrics().ErrorRequestBody; got != want {
		t.Fatalf("ErrorRequestBody = %d, want %d", got, want)
	}
}

// TestUpgradeDeclaredOversizeRefusedAtOnce976: an Upgrade: h2c request that
// declares a body over MaxRequestBodySize is refused with 413 as soon as its
// head is read, the body never sent: 32 MiB (the bound would have been about
// 4 KiB of it), just over the limit, and 8 KiB. ReadTimeout is off, so before
// the fix the server waited for ever for the bytes.
func TestUpgradeDeclaredOversizeRefusedAtOnce976(t *testing.T) {
	for _, proto := range []engine.Protocol{engine.Auto, engine.H2C} {
		for _, n := range []int64{32 << 20, 4097, 8192} {
			t.Run(fmt.Sprintf("%v-%d", proto, n), func(t *testing.T) {
				e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
					c.Protocol = proto
					c.SetH2Upgrade(true)
					c.MaxRequestBodySize = 4096
				})
				first, took := replyWithheld976(t, addr, upgradeHead976+fmt.Sprintf("Content-Length: %d\r\n\r\n", n), 2*time.Second)
				t.Logf("MEASURE %v declared=%d first=%q took=%v", proto, n, first, took.Round(time.Millisecond))
				if !strings.HasPrefix(first, "HTTP/1.1 413") {
					t.Fatalf("an Upgrade: h2c request declaring %d body bytes (MaxRequestBodySize 4096), body withheld, got %q after %v, want 413 at once", n, first, took)
				}
				waitRequestBodyError976(t, e, 1)
			})
		}
	}
}

// TestUpgradeUnknownLengthStillBoundedByRead976: with no Content-Length
// (chunked, -1) there is nothing to look at before the read, which stays the
// one that stops one byte past the limit: 6000 bytes of chunks, the last chunk
// never sent, are refused.
func TestUpgradeUnknownLengthStillBoundedByRead976(t *testing.T) {
	e, addr := startAutoEngineWith976(t, 4096)
	chunk := strings.Repeat("x", 3000)
	head := upgradeHead976 + "Transfer-Encoding: chunked\r\n\r\n" +
		fmt.Sprintf("%x\r\n%s\r\n%x\r\n%s\r\n", len(chunk), chunk, len(chunk), chunk)
	first, took := replyWithheld976(t, addr, head, 2*time.Second)
	if !strings.HasPrefix(first, "HTTP/1.1 413") {
		t.Fatalf("a chunked Upgrade: h2c body of 6000 bytes (MaxRequestBodySize 4096) got %q after %v, want 413", first, took)
	}
	waitRequestBodyError976(t, e, 1)
}

func startAutoEngineWith976(t *testing.T, limit int64) (*Engine, string) {
	t.Helper()
	return startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.Auto
		c.MaxRequestBodySize = limit
	})
}

// TestBridgeDeclaredOversizeRefusedAtOnceH1976: the same on a plain HTTP/1.1
// request through Bridge (Protocol HTTP1, no upgrade). 8 KiB is under what
// net/http reads to reuse a connection (256 KiB): the 413 has to say
// Connection: close, or net/http reads the withheld body before it answers.
// ReadTimeout is long, so a stall shows as no reply within 2 s.
func TestBridgeDeclaredOversizeRefusedAtOnceH1976(t *testing.T) {
	for _, n := range []int64{32 << 20, 4097, 8192} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
				c.Protocol = engine.HTTP1
				c.MaxRequestBodySize = 4096
				c.ReadTimeout = 30 * time.Second
			})
			head := fmt.Sprintf("POST /h HTTP/1.1\r\nHost: std.test\r\nContent-Length: %d\r\n\r\n", n)
			first, took := replyWithheld976(t, addr, head, 2*time.Second)
			t.Logf("MEASURE h1 declared=%d first=%q took=%v", n, first, took.Round(time.Millisecond))
			if !strings.HasPrefix(first, "HTTP/1.1 413") {
				t.Fatalf("an HTTP/1.1 request declaring %d body bytes (MaxRequestBodySize 4096), body withheld, got %q after %v, want 413 at once", n, first, took)
			}
			waitRequestBodyError976(t, e, 1)
		})
	}
}

// TestBridgeDeclaredOversizeRefusedAtOnceH2976: and on an h2c stream, whose
// HEADERS declare a content-length of 8 KiB and whose DATA never comes.
func TestBridgeDeclaredOversizeRefusedAtOnceH2976(t *testing.T) {
	e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.H2C
		c.MaxRequestBodySize = 4096
	})
	r := dialRawH2(t, addr, false)
	start := time.Now()
	r.sendHeaders(1, "POST", "/h", false, [2]string{"content-length", "8192"})
	ev, _, ok := r.waitFor(2*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders || ev.endsConn })
	t.Logf("MEASURE h2 ok=%v event=%+v took=%v", ok, ev, time.Since(start).Round(time.Millisecond))
	if !ok || ev.status != "413" {
		t.Fatalf("an h2c stream declaring 8192 body bytes (MaxRequestBodySize 4096), DATA withheld: ok=%v event=%+v, want a 413 HEADERS frame at once", ok, ev)
	}
	waitRequestBodyError976(t, e, 1)
}

// readCounter976 is a request body that counts what is asked of it.
type readCounter976 struct {
	r      io.Reader
	reads  int
	closed bool
}

func (b *readCounter976) Read(p []byte) (int, error) {
	b.reads++
	return b.r.Read(p)
}
func (b *readCounter976) Close() error { b.closed = true; return nil }

// TestBridgeDeclaredLengthDecidesBeforeTheRead976 calls Bridge.ServeHTTP
// directly, on HTTP/1 and HTTP/2: a Content-Length over the limit is a 413
// with no read of the body (and Connection: close on HTTP/1 only: on HTTP/2 it
// would end the whole connection), one byte over the limit too; a Content-Length
// exactly at the limit is served, as is a body with no Content-Length (-1),
// which keeps the read that stops one byte past the limit.
func TestBridgeDeclaredLengthDecidesBeforeTheRead976(t *testing.T) {
	const limit = 4096
	for _, major := range []int{1, 2} {
		for _, tc := range []struct {
			name       string
			cl         int64
			body       int
			wantStatus int
			wantReads  bool
		}{
			{"declared-over", limit + 1, 0, 413, false},
			{"declared-far-over", 1 << 30, 0, 413, false},
			{"declared-at-limit", limit, limit, 200, true},
			{"declared-under", 100, 100, 200, true},
			{"unknown-length-over", -1, limit + 1, 413, true},
			{"unknown-length-at-limit", -1, limit, 200, true},
		} {
			t.Run(fmt.Sprintf("h%d-%s", major, tc.name), func(t *testing.T) {
				e, err := New(resource.Config{Addr: "127.0.0.1:0", Engine: engine.Std, Protocol: engine.HTTP1, MaxRequestBodySize: limit}, okStreamHandler{})
				if err != nil {
					t.Fatal(err)
				}
				br := &Bridge{engine: e, handler: okStreamHandler{}}
				body := &readCounter976{r: bytes.NewReader(make([]byte, tc.body))}
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req.Body, req.ContentLength, req.ProtoMajor = body, tc.cl, major
				rec := httptest.NewRecorder()
				br.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status %d, want %d", rec.Code, tc.wantStatus)
				}
				if (body.reads > 0) != tc.wantReads {
					t.Fatalf("the body was read %d times, want reads: %v", body.reads, tc.wantReads)
				}
				if tc.wantStatus == 413 {
					if got := e.Metrics().ErrorRequestBody; got != 1 {
						t.Fatalf("ErrorRequestBody = %d after a refused body, want 1", got)
					}
				}
				if tc.name == "declared-over" || tc.name == "declared-far-over" {
					if got, want := rec.Header().Get("Connection") == "close", major == 1; got != want {
						t.Fatalf("Connection: close = %v on HTTP/%d, want %v", got, major, want)
					}
				}
			})
		}
	}
}

// TestUpgradeBadSettingsWinsOverDeclaredOversize976 pins the order in
// h2cUpgrade: the HTTP2-Settings header is checked before the declared length,
// so an upgrade request with an invalid header and a declared length over the
// limit is answered 500, not 413, and is not counted as a refused body.
func TestUpgradeBadSettingsWinsOverDeclaredOversize976(t *testing.T) {
	e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.H2C
		c.SetH2Upgrade(true)
		c.MaxRequestBodySize = 4096
	})
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	// The body is sent whole: the 500 does not close the connection, so
	// net/http reads the declared body before it writes the answer.
	if _, err := io.WriteString(c, "POST /h HTTP/1.1\r\nHost: std.test\r\nConnection: Upgrade, HTTP2-Settings\r\n"+
		"Upgrade: h2c\r\nHTTP2-Settings: !!not-base64!!\r\nContent-Length: 8192\r\n\r\n"+strings.Repeat("x", 8192)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(line, "HTTP/1.1 500") {
		t.Fatalf("an upgrade with an invalid HTTP2-Settings and a declared length over the limit was answered %q, want 500", strings.TrimSpace(line))
	}
	if got := e.Metrics().ErrorRequestBody; got != 0 {
		t.Fatalf("ErrorRequestBody = %d, want 0: the request was refused for its settings, not its body", got)
	}
}
