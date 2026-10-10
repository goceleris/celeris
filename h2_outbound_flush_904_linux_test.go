//go:build linux

package celeris_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#903 and #904 on the wire. h2Win904 is a raw h2c client that keeps
// its own books of the flow-control credit it has granted, and holds every DATA
// frame the server sends against them: a frame longer than the stream's
// remaining credit or the connection's is the violation RFC 9113 §6.9.1
// forbids (a strict client answers FLOW_CONTROL_ERROR). It also flags DATA that
// arrives before its stream's HEADERS (#903) and checks the bytes against the
// body893 pattern. Credit is counted before the WINDOW_UPDATE or SETTINGS that
// grants it is written, so a server can never be charged for a frame the
// client's books had not yet allowed.
type h2Win904 struct {
	conn net.Conn
	fr   *http2.Framer
	wmu  sync.Mutex
	enc  *hpack.Encoder
	hbuf bytes.Buffer
	addr string

	mu         sync.Mutex
	connCredit int64
	strCredit  map[uint32]int64
	initial    int64
	headers    map[uint32]bool
	data       map[uint32][]byte
	ended      map[uint32]bool
	resets     map[uint32]http2.ErrCode
	violations []string
	done       chan struct{}
	// replenish makes the reader grant back, stream and connection, every
	// DATA byte it receives, as a client that reads its responses does.
	replenish atomic.Bool
}

// dialWin904 connects and sends SETTINGS_INITIAL_WINDOW_SIZE initial.
func dialWin904(t *testing.T, addr string, initial uint32, replenish bool) *h2Win904 {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &h2Win904{conn: conn, addr: addr, connCredit: 65535, initial: int64(initial), strCredit: map[uint32]int64{},
		headers: map[uint32]bool{}, data: map[uint32][]byte{}, ended: map[uint32]bool{}, resets: map[uint32]http2.ErrCode{},
		done: make(chan struct{})}
	c.replenish.Store(replenish)
	t.Cleanup(func() { _ = conn.Close(); <-c.done })
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	c.fr = http2.NewFramer(conn, conn)
	c.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	c.enc = hpack.NewEncoder(&c.hbuf)
	c.wmu.Lock()
	err = c.fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: initial})
	c.wmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	go c.read()
	return c
}

func (c *h2Win904) violate(format string, args ...any) {
	if len(c.violations) < 5 {
		c.violations = append(c.violations, fmt.Sprintf(format, args...))
	}
}

func (c *h2Win904) read() {
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
			c.headers[f.StreamID] = true
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			c.mu.Unlock()
		case *http2.DataFrame:
			n := int64(len(f.Data()))
			c.mu.Lock()
			if !c.headers[f.StreamID] {
				c.violate("stream %d: DATA (%d bytes) before the stream's HEADERS (#903)", f.StreamID, n)
			}
			c.connCredit -= n
			c.strCredit[f.StreamID] -= n
			if c.connCredit < 0 {
				c.violate("stream %d: a DATA frame of %d bytes took the connection window to %d (RFC 9113 §6.9.1)", f.StreamID, n, c.connCredit)
			}
			if c.strCredit[f.StreamID] < 0 {
				c.violate("stream %d: a DATA frame of %d bytes took the stream window to %d (RFC 9113 §6.9.1)", f.StreamID, n, c.strCredit[f.StreamID])
			}
			c.data[f.StreamID] = append(c.data[f.StreamID], f.Data()...)
			if f.StreamEnded() {
				c.ended[f.StreamID] = true
			}
			if c.replenish.Load() && n > 0 {
				c.connCredit += n
				c.strCredit[f.StreamID] += n
			}
			c.mu.Unlock()
			if n > 0 && c.replenish.Load() {
				c.wmu.Lock()
				_ = c.fr.WriteWindowUpdate(0, uint32(n))
				if !f.StreamEnded() {
					_ = c.fr.WriteWindowUpdate(f.StreamID, uint32(n))
				}
				c.wmu.Unlock()
			}
		case *http2.RSTStreamFrame:
			c.mu.Lock()
			c.resets[f.StreamID] = f.ErrCode
			c.mu.Unlock()
		}
	}
}

func (c *h2Win904) get(t *testing.T, id uint32, path string) {
	t.Helper()
	c.mu.Lock()
	c.strCredit[id] = c.initial
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

// grant credits stream id (0 is the connection) with n bytes.
func (c *h2Win904) grant(t *testing.T, id uint32, n uint32) {
	t.Helper()
	c.mu.Lock()
	if id == 0 {
		c.connCredit += int64(n)
	} else {
		c.strCredit[id] += int64(n)
	}
	c.mu.Unlock()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.fr.WriteWindowUpdate(id, n); err != nil {
		t.Fatalf("WINDOW_UPDATE %d +%d: %v", id, n, err)
	}
}

// setInitial sends SETTINGS_INITIAL_WINDOW_SIZE n, which moves every open
// stream's window by the difference (RFC 9113 §6.9.2).
func (c *h2Win904) setInitial(t *testing.T, n uint32) {
	t.Helper()
	c.mu.Lock()
	delta := int64(n) - c.initial
	c.initial = int64(n)
	for id := range c.strCredit {
		c.strCredit[id] += delta
	}
	c.mu.Unlock()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: n}); err != nil {
		t.Fatal(err)
	}
}

func (c *h2Win904) received(id uint32) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data[id])
}

// settled waits until stream id's received bytes have not changed for quiet.
func (c *h2Win904) settled(id uint32, quiet time.Duration) int {
	last, since := -1, time.Now()
	for until := time.Now().Add(15 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		if n := c.received(id); n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= quiet {
			return n
		}
	}
	return c.received(id)
}

// check fails t unless stream id arrived whole, in order, ended, with no
// violation.
func (c *h2Win904) check(t *testing.T, id uint32, want []byte) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := c.ended[id]
		c.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.violations) > 0 {
		t.Errorf("flow-control or ordering violations: %v", c.violations)
	}
	if !c.ended[id] || !bytes.Equal(c.data[id], want) {
		t.Errorf("stream %d: %d of %d body bytes, END_STREAM %v, in order %v, resets %v", id, len(c.data[id]), len(want), c.ended[id],
			bytes.HasPrefix(want, c.data[id]), c.resets)
	}
}

func streamBody904(body []byte) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		sw := c.StreamWriter()
		if sw == nil {
			return errors.New("no StreamWriter")
		}
		if err := sw.WriteHeader(200, [][2]string{{"content-type", "application/octet-stream"}}); err != nil {
			return err
		}
		if _, err := sw.Write(body); err != nil {
			return err
		}
		return sw.Close()
	}
}

// TestH2SendPathsHonourThePeersWindows904 runs the send paths of #903 and
// #904 against a client that checks every DATA frame against the windows it
// granted (and its HEADERS-first order), on every engine: a StreamWriter
// response on the worker pool and inline, with the default windows and with a
// 4,096-byte stream window; and a pool handler's buffered c.Blob whose
// SETTINGS_INITIAL_WINDOW_SIZE is raised while the connection window is used up.
func TestH2SendPathsHonourThePeersWindows904(t *testing.T) {
	body := body893(300000)
	small := body893(100000)
	for _, e := range engines893 {
		serve := func(t *testing.T) string {
			return startServerConfig761(t, celeris.Config{Engine: e.eng}, func(s *celeris.Server) {
				s.GET("/sw-async", streamBody904(body)).Async()
				s.GET("/sw-small", streamBody904(small)).Async()
				s.GET("/sw-inline", streamBody904(body))
				s.GET("/blob-async", func(c *celeris.Context) error {
					return c.Blob(200, "application/octet-stream", body)
				}).Async()
			})
		}
		t.Run(e.name+"/streamwriter-async", func(t *testing.T) {
			c := dialWin904(t, serve(t), 65535, true)
			c.get(t, 1, "/sw-async")
			c.check(t, 1, body)
		})
		t.Run(e.name+"/streamwriter-small-window", func(t *testing.T) {
			c := dialWin904(t, serve(t), 4096, true)
			c.get(t, 1, "/sw-small")
			c.check(t, 1, small)
		})
		t.Run(e.name+"/streamwriter-inline", func(t *testing.T) {
			c := dialWin904(t, serve(t), 65535, true)
			c.get(t, 1, "/sw-inline")
			c.check(t, 1, body)
		})
		t.Run(e.name+"/settings-raise-with-connection-window-used-up", func(t *testing.T) {
			c := dialWin904(t, serve(t), 65535, false)
			c.get(t, 1, "/blob-async")
			if n := c.settled(1, 300*time.Millisecond); n != 65535 {
				t.Fatalf("with no WINDOW_UPDATE: %d DATA bytes before the SETTINGS, want 65535 (violations %v)", n, c.violations)
			}
			// +100,000 on the stream window; the connection window gets nothing.
			c.setInitial(t, 65535+100000)
			if n := c.settled(1, 300*time.Millisecond); n != 65535 {
				c.mu.Lock()
				t.Errorf("after SETTINGS_INITIAL_WINDOW_SIZE +100000 and no connection WINDOW_UPDATE: %d DATA bytes in all, want 65535 (violations %v)", n, c.violations)
				c.mu.Unlock()
			}
			c.replenish.Store(true)
			c.grant(t, 0, 400000)
			c.grant(t, 1, 400000)
			c.check(t, 1, body)
		})
	}
}

// TestH2LockHeldAcrossAWaitingStreamWriteDoesNotWedgeTheServer904 is
// TestH2LockHeldAcrossAWaitingWriteDoesNotWedgeTheServer893 with a
// StreamWriter route (the constraint #904 puts on its fix): /hold takes a
// mutex and streams its response through c.StreamWriter() while holding it,
// /locked takes the same mutex. A slow h2c client over the budget GETs /hold
// (on the pool, for the budget), whose Write now waits for window. On epoll,
// io_uring and adaptive the /locked handlers wait for the lock on their event
// loops, so that wait must end at WriteTimeout (1 s here), as a pool
// handler's WriteResponse does, or the loops cannot read the WINDOW_UPDATE or
// the close that would end it. Every request must be answered while the slow
// client is connected and after it closes, and the server must stop.
func TestH2LockHeldAcrossAWaitingStreamWriteDoesNotWedgeTheServer904(t *testing.T) {
	const timeout = time.Second
	fill, val := body893(waitBodyLen893), body893(256<<10)
	for _, e := range engines893 {
		t.Run(e.name, func(t *testing.T) {
			var mu sync.Mutex
			var holdErr atomic.Value
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, WriteTimeout: timeout}, func(s *celeris.Server) {
				s.GET("/fill", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", fill) })
				s.GET("/hold", func(c *celeris.Context) error {
					mu.Lock()
					defer mu.Unlock()
					err := streamBody904(val)(c)
					if err != nil {
						holdErr.Store(err.Error())
					}
					return err
				})
				s.GET("/locked", func(c *celeris.Context) error {
					mu.Lock()
					mu.Unlock() // waiting for the lock is the point
					return c.String(200, "ok")
				})
			})
			slow := overBudget893(t, addr, fill)
			slow.get(t, 9, "/hold", val)
			if !slow.waitFor(5*time.Second, func() bool { return slow.headers[9] != "" }) {
				t.Fatalf("%s: no HEADERS for /hold within 5 s", e.name)
			}
			time.Sleep(50 * time.Millisecond)
			var lockers, bystanders []string
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); lockers = get893(8, "http://"+addr+"/locked", timeout+4*time.Second) }()
			go func() {
				defer wg.Done()
				time.Sleep(100 * time.Millisecond)
				bystanders = get893(8, "http://"+addr+"/ping", timeout+4*time.Second)
			}()
			wg.Wait()
			slow.mu.Lock()
			rst, reset := slow.resets[9]
			slow.mu.Unlock()
			_ = slow.conn.Close()
			time.Sleep(100 * time.Millisecond)
			var afterPing, afterLock []string
			wg.Add(2)
			go func() { defer wg.Done(); afterPing = get893(8, "http://"+addr+"/ping", 5*time.Second) }()
			go func() { defer wg.Done(); afterLock = get893(8, "http://"+addr+"/locked", 5*time.Second) }()
			wg.Wait()
			nl, sl := answered893(lockers)
			nb, sb := answered893(bystanders)
			np, sp := answered893(afterPing)
			na, sa := answered893(afterLock)
			t.Logf("%s: WriteTimeout %v; /hold reset=%v %v, error %v; while /hold waited: /locked %s, /ping %s; after the slow client closed: /ping %s, /locked %s",
				e.name, timeout, reset, rst, holdErr.Load(), sl, sb, sp, sa)
			if nl != 8 || nb != 8 {
				t.Errorf("%s: while /hold waited for window with the lock held, /locked answered %d/8 and /ping %d/8, want 8/8 each (the event loops must not wait past WriteTimeout)", e.name, nl, nb)
			}
			if np != 8 || na != 8 {
				t.Errorf("%s: after the slow client closed, /ping answered %d/8 and /locked %d/8, want 8/8 each", e.name, np, na)
			}
		})
	}
}
