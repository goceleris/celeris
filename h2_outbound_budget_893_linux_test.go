//go:build linux

package celeris_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/swagger"
)

// body893 is a patterned body: byte i is 'a'+i%26, so a byte out of place
// does not compare equal.
func body893(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func heapInuse893() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// h2Client893 is a raw h2c client (prior knowledge) that keeps the default
// 65,535-byte windows and grants no WINDOW_UPDATE until credit is called. It
// tallies, per stream, the response HEADERS, the DATA bytes (checked against
// the body893 pattern as they arrive) and END_STREAM.
type h2Client893 struct {
	conn net.Conn
	fr   *http2.Framer
	wmu  sync.Mutex
	enc  *hpack.Encoder
	hbuf bytes.Buffer
	addr string

	mu      sync.Mutex
	headers map[uint32]string // :status
	data    map[uint32]int
	ended   map[uint32]bool
	bad     map[uint32]string // first byte that differs from the stream's want
	wants   map[uint32][]byte
	resets  map[uint32]http2.ErrCode
	goAway  string
	done    chan struct{}
	// replenish makes the reader grant back, stream and connection, every
	// DATA byte it receives, as a client that reads its responses does.
	replenish atomic.Bool
}

func dialH2Client893(t *testing.T, addr string) *h2Client893 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &h2Client893{conn: conn, addr: addr, headers: map[uint32]string{}, data: map[uint32]int{},
		ended: map[uint32]bool{}, bad: map[uint32]string{}, resets: map[uint32]http2.ErrCode{}, wants: map[uint32][]byte{},
		done: make(chan struct{})}
	t.Cleanup(func() { _ = conn.Close(); <-c.done })
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	c.fr = http2.NewFramer(conn, conn)
	c.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	c.enc = hpack.NewEncoder(&c.hbuf)
	c.wmu.Lock()
	err = c.fr.WriteSettings()
	c.wmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	go c.read()
	return c
}

func (c *h2Client893) read() {
	defer close(c.done)
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				c.wmu.Lock()
				_ = c.fr.WriteSettingsAck()
				c.wmu.Unlock()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				c.wmu.Lock()
				_ = c.fr.WritePing(true, f.Data)
				c.wmu.Unlock()
			}
		case *http2.MetaHeadersFrame:
			c.mu.Lock()
			c.headers[f.StreamID] = f.PseudoValue("status")
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			c.mu.Unlock()
		case *http2.DataFrame:
			c.mu.Lock()
			off := c.data[f.StreamID]
			if _, seen := c.bad[f.StreamID]; !seen {
				want := c.wants[f.StreamID]
				for j, b := range f.Data() {
					if off+j >= len(want) {
						c.bad[f.StreamID] = fmt.Sprintf("byte %d is past the %d-byte body", off+j, len(want))
						break
					}
					if b != want[off+j] {
						c.bad[f.StreamID] = fmt.Sprintf("byte %d is %q, want %q", off+j, b, want[off+j])
						break
					}
				}
			}
			c.data[f.StreamID] = off + len(f.Data())
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			c.mu.Unlock()
			if n := uint32(len(f.Data())); n > 0 && c.replenish.Load() {
				c.wmu.Lock()
				_ = c.fr.WriteWindowUpdate(0, n)
				if !f.StreamEnded() {
					_ = c.fr.WriteWindowUpdate(f.StreamID, n)
				}
				c.wmu.Unlock()
			}
		case *http2.RSTStreamFrame:
			c.mu.Lock()
			c.resets[f.StreamID] = f.ErrCode
			c.mu.Unlock()
		case *http2.GoAwayFrame:
			c.mu.Lock()
			c.goAway = fmt.Sprintf("%v %q", f.ErrCode, f.DebugData())
			c.mu.Unlock()
		}
	}
}

func (c *h2Client893) get(t *testing.T, id uint32, path string, want []byte) {
	t.Helper()
	c.mu.Lock()
	c.wants[id] = want
	c.mu.Unlock()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.hbuf.Reset()
	for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: c.addr}, {Name: ":path", Value: path}} {
		_ = c.enc.WriteField(hf)
	}
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: c.hbuf.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatalf("stream %d: %v", id, err)
	}
}

func (c *h2Client893) credit(t *testing.T, id uint32, n uint32) {
	t.Helper()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.fr.WriteWindowUpdate(id, n); err != nil {
		t.Fatalf("WINDOW_UPDATE %d +%d: %v", id, n, err)
	}
}

// waitFor polls cond (under c.mu) until it holds or d passes.
func (c *h2Client893) waitFor(d time.Duration, cond func() bool) bool {
	for until := time.Now().Add(d); ; {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(until) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var engines893 = []struct {
	name string
	eng  celeris.EngineType
}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}

// TestH2SilentClientCannotPinResponseBodies893 is celeris#893. An HTTP/2
// client that opens many streams for a large response and never sends
// WINDOW_UPDATE made epoll, io_uring and adaptive copy the unsent part of
// every stream's body into memory and keep it until the connection closed:
// about 150 MiB for 99 streams of a 1.5 MiB body, per connection. std held
// about 1 MiB: its handlers block on flow control instead.
//
// The targets: the embedded Swagger UI bundle (1,586,002 bytes), which #851
// serves by default at a fixed URL, and a route answering c.Blob with a
// 512 KiB body, inline (sync) and on the worker pool (async). 99 such bodies
// are 50 MiB, still twice the bound, and keep the second phase short.
//
// Phase 1 measures the server's heap growth (the server runs in this
// process; each body is one shared slice, so a handler holds no copy of its
// own) 1 s after every stream got its HEADERS, against a bound of
// pinnedBound893. Phase 2, on the native engines' routes, then reads,
// granting back every byte as it arrives, and every stream must arrive whole
// and in order: what the fix holds back must still be sent. (The bundle's
// handler sends it the way the sync route does; std's flow control is not
// under test.)
func TestH2SilentClientCannotPinResponseBodies893(t *testing.T) {
	const (
		streams        = 99
		pinnedBound893 = 16 << 20
	)
	bundlePath := "/swagger/assets/swagger-ui-dist@" + swagger.SwaggerUIVersion + "/swagger-ui-bundle.js"
	bundle, err := os.ReadFile(filepath.Join("middleware", "swagger", "assets", "swagger-ui-dist", "swagger-ui-bundle.js"))
	if err != nil {
		t.Fatal(err)
	}
	big := body893(512 << 10)
	targets := []struct {
		name, path string
		body       []byte
	}{{"swagger-bundle", bundlePath, bundle}, {"sync", "/big893", big}, {"async", "/big893", big}}
	for _, e := range engines893 {
		for _, target := range targets {
			bodyLen := len(target.body)
			t.Run(e.name+"/"+target.name, func(t *testing.T) {
				route := target.name
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					r := s.GET("/big893", func(c *celeris.Context) error {
						return c.Blob(200, "application/octet-stream", big)
					})
					if route == "async" {
						r.Async()
					}
					if route == "swagger-bundle" {
						s.Pre(swagger.New(swagger.Config{SpecContent: []byte(`{"openapi":"3.0.0","info":{"title":"893","version":"1"},"paths":{}}`)}))
					}
				})
				h0 := heapInuse893()
				c := dialH2Client893(t, addr)
				for i := 0; i < streams; i++ {
					c.get(t, uint32(2*i+1), target.path, target.body)
				}
				if !c.waitFor(15*time.Second, func() bool { return len(c.headers) == streams }) {
					c.mu.Lock()
					t.Fatalf("%s/%s: HEADERS for %d of %d streams within 15 s (resets %v, GOAWAY %q)",
						e.name, route, len(c.headers), streams, c.resets, c.goAway)
				}
				time.Sleep(time.Second)
				h1 := heapInuse893()
				c.mu.Lock()
				sent := 0
				for _, n := range c.data {
					sent += n
				}
				c.mu.Unlock()
				growth := int64(h1) - int64(h0)
				t.Logf("%s/%s: %d streams of %d bytes, no WINDOW_UPDATE: HEADERS on all, %d DATA bytes received, server heap growth %.1f MiB (bound %.0f MiB)",
					e.name, route, streams, bodyLen, sent, float64(growth)/(1<<20), float64(pinnedBound893)/(1<<20))
				if growth > pinnedBound893 {
					t.Errorf("%s/%s: the server holds %.1f MiB for one connection that grants no window (celeris#893), bound %.0f MiB",
						e.name, route, float64(growth)/(1<<20), float64(pinnedBound893)/(1<<20))
				}

				c.mu.Lock()
				for id, st := range c.headers {
					if st != "200" {
						t.Errorf("%s/%s: stream %d answered %s", e.name, route, id, st)
					}
				}
				c.mu.Unlock()
				if e.name == "std" || route == "swagger-bundle" {
					return
				}
				// Phase 2: the client reads from now on, granting back every
				// byte it receives (and what it received before, plus 4 MiB
				// of connection window to start with). Every stream must
				// complete intact.
				c.replenish.Store(true)
				c.mu.Lock()
				got := make(map[uint32]int, len(c.data))
				for id, n := range c.data {
					got[id] = n
				}
				c.mu.Unlock()
				c.credit(t, 0, 4<<20)
				for id, n := range got {
					if n > 0 {
						c.credit(t, id, uint32(n))
					}
				}
				ok := c.waitFor(90*time.Second, func() bool {
					for i := 0; i < streams; i++ {
						if id := uint32(2*i + 1); !c.ended[id] || c.data[id] != bodyLen {
							return false
						}
					}
					return true
				})
				c.mu.Lock()
				defer c.mu.Unlock()
				if !ok || len(c.bad) > 0 || len(c.resets) > 0 || c.goAway != "" {
					short := 0
					for i := 0; i < streams; i++ {
						if id := uint32(2*i + 1); !c.ended[id] || c.data[id] != bodyLen {
							short++
						}
					}
					t.Fatalf("%s/%s: after the windows opened, %d of %d streams incomplete, pattern errors %v, resets %v, GOAWAY %q",
						e.name, route, short, streams, c.bad, c.resets, c.goAway)
				}
			})
		}
	}
}

// TestH2SlowDownloadDoesNotHoldBackOtherStreams893 is the other side of
// celeris#893's bound: holding back a connection that is over its budget must
// not make its other streams wait for that download (std answers them at
// once). The client grants connection window but never stream window to an
// 8 MiB download, so the server holds most of it, over the budget; a small
// GET on the same connection must then still be answered in full, at once.
func TestH2SlowDownloadDoesNotHoldBackOtherStreams893(t *testing.T) {
	const bigLen = 8 << 20
	big, small := body893(bigLen), body893(1000)
	for _, e := range engines893 {
		for _, route := range []string{"sync", "async"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					rb := s.GET("/big893", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", big) })
					rs := s.GET("/small893", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", small) })
					if route == "async" {
						rb.Async()
						rs.Async()
					}
				})
				c := dialH2Client893(t, addr)
				c.credit(t, 0, 64<<20)
				c.get(t, 1, "/big893", big)
				if !c.waitFor(10*time.Second, func() bool { return c.data[1] >= 65535 }) {
					c.mu.Lock()
					t.Fatalf("%s/%s: the download's first window never arrived (%d bytes)", e.name, route, c.data[1])
				}
				time.Sleep(200 * time.Millisecond)
				start := time.Now()
				c.get(t, 3, "/small893", small)
				ok := c.waitFor(5*time.Second, func() bool { return c.ended[3] })
				el := time.Since(start)
				c.mu.Lock()
				defer c.mu.Unlock()
				t.Logf("%s/%s: download stalled at %d of %d bytes; the small GET ended=%v with %d bytes after %v",
					e.name, route, c.data[1], bigLen, ok, c.data[3], el.Round(time.Millisecond))
				if !ok || c.data[3] != len(small) || c.headers[3] != "200" || c.bad[3] != "" {
					t.Errorf("%s/%s: the small GET behind a stalled download: status %q, %d of %d bytes, ended %v, pattern %q, resets %v",
						e.name, route, c.headers[3], c.data[3], len(small), c.ended[3], c.bad[3], c.resets)
				}
			})
		}
	}
}
