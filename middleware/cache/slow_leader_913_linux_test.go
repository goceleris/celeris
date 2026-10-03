//go:build linux

package cache_test

// celeris#913: the coalesced fill's leader wrote its response to its own
// client inside the coalesced call, so every follower of the key waited for
// that client. A client that reads slowly (an HTTP/2 client that grants no
// window) held them all. On std the followers stall. On epoll, io_uring and
// adaptive the same happens once a handler's write waits for the peer's
// window (celeris#893's outbound budget), and there a sync follower holds its
// event loop too, and with it every connection on that loop. With a bounded
// write (WriteTimeout) the leader's write error became the coalesced call's
// result instead, and the followers answered it as a 500.
//
// singleflight.New() is the control: it releases its waiters before its
// leader writes, so its rows pass with or without the fix, which shows the
// slow client alone does not stall a follower.

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/singleflight"
)

var engines913 = []struct {
	name string
	eng  celeris.EngineType
}{{"std", celeris.Std}, {"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}, {"adaptive", celeris.Adaptive}}

// TestCacheFollowersDoNotWaitForTheLeadersClient913: a slow HTTP/2 client is
// the leader of a coalesced fill. Its connection is first given four 1.5 MiB
// downloads it never reads, which use up its window (and, with #893, put it
// over the outbound budget), so its response to the key cannot be written.
// Two waves of followers on fresh HTTP/1.1 connections GET the key: one while
// the leader's handler runs, one after it returned, while the leader's
// response still waits for its client. Every follower must get the whole
// response, from the leader's one handler run, and requests that share
// nothing with the key must still be answered. Then the slow client reads,
// and must get its own response, marked MISS. With a 1 s WriteTimeout the
// leader's write fails instead, and that failure must stay the leader's.
func TestCacheFollowersDoNotWaitForTheLeadersClient913(t *testing.T) {
	big := pattern913(1536 << 10)
	val := pattern913(256 << 10)
	for _, e := range engines913 {
		for _, mw := range []string{"cache", "singleflight"} {
			for _, wt := range []time.Duration{0, time.Second} {
				name := e.name + "/" + mw + "/write-timeout-default"
				if wt > 0 {
					name = e.name + "/" + mw + "/write-timeout-" + wt.String()
				}
				t.Run(name, func(t *testing.T) { slowLeader913(t, e.eng, mw, wt, big, val) })
			}
		}
	}
}

func slowLeader913(t *testing.T, eng celeris.EngineType, mw string, writeTimeout time.Duration, big, val []byte) {
	var m celeris.HandlerFunc
	hitHeader := "x-cache"
	if mw == "cache" {
		m = cache.New()
	} else {
		m = singleflight.New()
		hitHeader = "x-singleflight"
	}
	var runs atomic.Int32
	entered := make(chan struct{})  // the first run of the key's handler started
	returned := make(chan struct{}) // and returned
	addr := startServer913(t, eng, writeTimeout, func(s *celeris.Server) {
		s.GET("/big", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", big) })
		s.GET("/popular", m, func(c *celeris.Context) error {
			first := runs.Add(1) == 1
			if first {
				close(entered)
			}
			time.Sleep(50 * time.Millisecond) // a backend call
			err := c.Blob(200, "application/octet-stream", val)
			if first {
				close(returned)
			}
			return err
		})
	})

	slow := dialH2Client913(t, addr)
	for id := uint32(1); id <= 7; id += 2 {
		slow.get(t, id, "/big")
	}
	if !slow.waitFor(10*time.Second, func() bool { return len(slow.status) == 4 }) {
		t.Fatalf("slow client: response HEADERS for %d of its 4 downloads", slow.count())
	}
	const leader = 9
	slow.get(t, leader, "/popular")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow client's request for the key never reached its handler")
	}

	const n = 8
	url := "http://" + addr
	wave1 := make([]answer913, n)
	wave2 := make([]answer913, n)
	others := make([]answer913, n)
	var wg sync.WaitGroup
	fetchAll913(&wg, wave1, url+"/popular", 3*time.Second, hitHeader)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		wg.Wait()
		t.Fatal("the leader's handler did not return within 5 s")
	}
	// Past the coalesced call: the second wave can only be answered from the
	// store, or from a call still held open by the leader's write.
	time.Sleep(20 * time.Millisecond)
	fetchAll913(&wg, wave2, url+"/popular", 3*time.Second, hitHeader)
	fetchAll913(&wg, others, url+"/ping", 2*time.Second, "")
	wg.Wait()

	check := func(wave string, as []answer913, want []byte, hit string) {
		t.Helper()
		bad := 0
		for i, a := range as {
			switch {
			case a.err != nil:
				t.Errorf("%s %d: no response after %v: %v", wave, i, a.took, a.err)
			case a.status != 200:
				t.Errorf("%s %d: status %d after %v, want 200", wave, i, a.status, a.took)
			case !bytes.Equal(a.body, want):
				t.Errorf("%s %d: %d-byte body, want the handler's %d bytes", wave, i, len(a.body), len(want))
			case hit != "" && a.hit != hit:
				t.Errorf("%s %d: %s %q, want %q", wave, i, hitHeader, a.hit, hit)
			default:
				continue
			}
			bad++
		}
		t.Logf("%s: %d of %d answered as wanted", wave, len(as)-bad, len(as))
	}
	cacheHit := ""
	if mw == "cache" {
		cacheHit = "HIT"
	}
	check("followers while the leader's handler runs", wave1, val, cacheHit)
	check("followers after the leader's handler returned", wave2, val, cacheHit)
	check("requests for another route", others, []byte("ok"), "")
	if mw == "cache" {
		// The fill is stored before the leader's write, so a follower that
		// comes after the call is a HIT, not a second run.
		if r := runs.Load(); r != 1 {
			t.Errorf("the key's handler ran %d times for the leader and %d followers, want 1", r, 2*n)
		}
	}

	if writeTimeout > 0 {
		return // the leader's write fails at its deadline; nothing more to read
	}
	// The slow client reads now: its own response must arrive whole.
	slow.open(t, 1, 3, 5, 7, leader)
	if !slow.waitFor(10*time.Second, func() bool { return slow.ended[leader] || slow.resets[leader] != 0 }) {
		t.Fatalf("leader: no END_STREAM within 10 s of the window opening (%d of %d body bytes)", slow.bodyLen(leader), len(val))
	}
	slow.mu.Lock()
	defer slow.mu.Unlock()
	var got []byte
	if b := slow.body[leader]; b != nil {
		got = b.Bytes()
	}
	if code := slow.resets[leader]; code != 0 {
		t.Fatalf("leader: stream reset (%v) after %d body bytes", code, len(got))
	}
	if st := slow.status[leader]; st != "200" {
		t.Errorf("leader: status %s, want 200", st)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("leader: %d-byte body, want the handler's %d bytes", len(got), len(val))
	}
	if mw == "cache" && slow.hit[leader] != "MISS" {
		t.Errorf("leader: x-cache %q, want MISS", slow.hit[leader])
	}
}

// pattern913 is a patterned body: byte i is 'a'+i%26.
func pattern913(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

type answer913 struct {
	status int
	hit    string
	body   []byte
	err    error
	took   time.Duration
}

// fetchAll913 GETs url once per slot of out, concurrently, each on a fresh
// HTTP/1.1 connection.
func fetchAll913(wg *sync.WaitGroup, out []answer913, url string, timeout time.Duration, hitHeader string) {
	for i := range out {
		wg.Go(func() {
			cl := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
			start := time.Now()
			resp, err := cl.Get(url)
			if err != nil {
				out[i] = answer913{err: err, took: time.Since(start)}
				return
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			a := answer913{status: resp.StatusCode, body: body, err: err, took: time.Since(start)}
			if hitHeader != "" {
				a.hit = resp.Header.Get(hitHeader)
			}
			out[i] = a
		})
	}
}

// startServer913 starts a server with routes and waits until it answers
// /ping. An io_uring start that fails only with ENOMEM is retried for up to
// 30 s: the kernel charges ring memory to RLIMIT_MEMLOCK per UID and gives it
// back some milliseconds after a ring closes, so at the CI runner's 8 MiB a
// start made while another package's test binary holds rings can fail
// although nothing leaked.
func startServer913(t *testing.T, eng celeris.EngineType, writeTimeout time.Duration, routes func(*celeris.Server)) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Engine: eng, Addr: addr, WriteTimeout: writeTimeout})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") })
		routes(s)
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady913(addr, startDone)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15 s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

func waitReady913(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("Start returned nil before the server was ready")
			}
			return err
		default:
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15 s", addr)
}

// h2Client913 is a raw h2c (prior knowledge) client that keeps the default
// 65,535-byte windows and grants no WINDOW_UPDATE until open is called: a
// client that reads slowly. Per stream it records :status, the hit header,
// the body and END_STREAM or RST_STREAM.
type h2Client913 struct {
	conn net.Conn
	addr string
	fr   *http2.Framer
	wmu  sync.Mutex
	enc  *hpack.Encoder
	hbuf bytes.Buffer

	mu     sync.Mutex
	status map[uint32]string
	hit    map[uint32]string
	body   map[uint32]*bytes.Buffer
	ended  map[uint32]bool
	resets map[uint32]http2.ErrCode
	done   chan struct{}
}

func dialH2Client913(t *testing.T, addr string) *h2Client913 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &h2Client913{conn: conn, addr: addr, status: map[uint32]string{}, hit: map[uint32]string{},
		body: map[uint32]*bytes.Buffer{}, ended: map[uint32]bool{}, resets: map[uint32]http2.ErrCode{},
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

func (c *h2Client913) read() {
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
			c.status[f.StreamID] = f.PseudoValue("status")
			for _, hf := range f.RegularFields() {
				if hf.Name == "x-cache" || hf.Name == "x-singleflight" {
					c.hit[f.StreamID] = hf.Value
				}
			}
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			c.mu.Unlock()
		case *http2.DataFrame:
			c.mu.Lock()
			b := c.body[f.StreamID]
			if b == nil {
				b = &bytes.Buffer{}
				c.body[f.StreamID] = b
			}
			b.Write(f.Data())
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			c.mu.Unlock()
		case *http2.RSTStreamFrame:
			c.mu.Lock()
			c.resets[f.StreamID] = f.ErrCode
			c.mu.Unlock()
		}
	}
}

func (c *h2Client913) get(t *testing.T, id uint32, path string) {
	t.Helper()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.hbuf.Reset()
	for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: c.addr}, {Name: ":path", Value: path}} {
		if err := c.enc.WriteField(hf); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: c.hbuf.Bytes(),
		EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
}

// open grants the connection and each of ids a window large enough for every
// response the test asks for.
func (c *h2Client913) open(t *testing.T, ids ...uint32) {
	t.Helper()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for _, id := range append([]uint32{0}, ids...) {
		if err := c.fr.WriteWindowUpdate(id, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *h2Client913) waitFor(timeout time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(timeout); ; {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *h2Client913) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.status)
}

func (c *h2Client913) bodyLen(id uint32) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.body[id]; b != nil {
		return b.Len()
	}
	return 0
}
