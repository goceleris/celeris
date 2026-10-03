//go:build linux

package celeris_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
)

// celeris#893, #906's review round 2. Over its connection's outbound budget a
// pool handler waits for the peer's window (sendRest, AwaitSendWindow). These
// pin the bound on that wait, the server's WriteTimeout, on the wire. The
// client first puts its connection over the budget: four GETs of a 1.5 MiB
// body on the default 65,535-byte windows, which it never credits.

const waitBodyLen893 = 1536 << 10

// overBudget893 dials a raw h2c client and puts its connection over the
// outbound budget with four GETs of /fill (streams 1, 3, 5, 7).
func overBudget893(t *testing.T, addr string, fill []byte) *h2Client893 {
	t.Helper()
	c := dialH2Client893(t, addr)
	for i := 0; i < 4; i++ {
		c.get(t, uint32(2*i+1), "/fill", fill)
	}
	if !c.waitFor(10*time.Second, func() bool { return len(c.headers) == 4 }) {
		c.mu.Lock()
		t.Fatalf("HEADERS for %d of the 4 fill streams within 10 s", len(c.headers))
	}
	return c
}

// TestH2HandlerWaitingForWindowIsBoundedByWriteTimeout893: a handler that
// waits for window must give up at the server's WriteTimeout and reset its
// stream with INTERNAL_ERROR, as std does (its per-stream write deadline),
// while the connection stays open. The client keeps the connection active
// with a PING every 200 ms and grants no window, so the connection's read and
// idle timeouts (set as short as WriteTimeout) never fire: before the bound,
// the native engines' handler waited until the client granted window.
func TestH2HandlerWaitingForWindowIsBoundedByWriteTimeout893(t *testing.T) {
	const timeout = time.Second
	fill, victim := body893(waitBodyLen893), body893(256<<10)
	for _, e := range engines893 {
		t.Run(e.name, func(t *testing.T) {
			var startedNs, returnedNs atomic.Int64
			var retErr atomic.Value
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, ReadTimeout: timeout, WriteTimeout: timeout, IdleTimeout: timeout}, func(s *celeris.Server) {
				s.GET("/fill", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", fill) })
				s.GET("/victim", func(c *celeris.Context) error {
					startedNs.Store(time.Now().UnixNano())
					err := c.Blob(200, "application/octet-stream", victim)
					retErr.Store(fmt.Sprint(err))
					returnedNs.Store(time.Now().UnixNano())
					return err
				})
			})
			c := overBudget893(t, addr, fill)
			c.get(t, 9, "/victim", victim)
			// Until the handler has returned and the client has seen the
			// reset, or WriteTimeout + 4 s.
			pings := 0
			for until := time.Now().Add(timeout + 4*time.Second); time.Now().Before(until); pings++ {
				c.mu.Lock()
				_, reset := c.resets[9]
				c.mu.Unlock()
				if reset && returnedNs.Load() != 0 {
					break
				}
				c.wmu.Lock()
				_ = c.fr.WritePing(false, [8]byte{'8', '9', '3'})
				c.wmu.Unlock()
				time.Sleep(200 * time.Millisecond)
			}
			st, rt := startedNs.Load(), returnedNs.Load()
			alive := true
			select {
			case <-c.done:
				alive = false
			default:
			}
			c.mu.Lock()
			rst, reset := c.resets[9]
			got, ended, goAway, bad := c.data[9], c.ended[9], c.goAway, c.bad[9]
			c.mu.Unlock()
			errs, _ := retErr.Load().(string)
			took := "not returned"
			if rt != 0 {
				took = "returned after " + time.Duration(rt-st).Round(10*time.Millisecond).String()
			}
			t.Logf("%s: WriteTimeout = ReadTimeout = IdleTimeout = %v, %d PINGs, no WINDOW_UPDATE: /victim's handler started=%v, %s (err %s); stream 9: %d DATA bytes, ended=%v, reset=%v %v; connection open=%v, GOAWAY %q",
				e.name, timeout, pings, st != 0, took, errs, got, ended, reset, rst, alive, goAway)
			if rt == 0 {
				// Let it finish, so the server can stop.
				c.replenish.Store(true)
				c.credit(t, 0, 1<<30)
				c.credit(t, 9, 1<<30)
				t.Fatalf("%s: /victim's handler still waited for window %v after its start, WriteTimeout %v (#906 review round 2)",
					e.name, timeout+4*time.Second, timeout)
			}
			if d := time.Duration(rt - st); d > timeout+3*time.Second {
				t.Errorf("%s: /victim's handler returned %v after its start, WriteTimeout %v", e.name, d, timeout)
			}
			if !reset || rst != http2.ErrCodeInternal {
				t.Errorf("%s: stream 9 reset=%v %v, want RST_STREAM INTERNAL_ERROR", e.name, reset, rst)
			}
			if ended || got >= len(victim) || bad != "" {
				t.Errorf("%s: stream 9 ended=%v with %d of %d bytes (pattern %q): it should be cut off, not completed", e.name, ended, got, len(victim), bad)
			}
			if !alive || goAway != "" {
				t.Errorf("%s: the connection closed (open=%v, GOAWAY %q): the bound resets the stream, not the connection", e.name, alive, goAway)
			}
		})
	}
}

// get893 GETs url from n fresh HTTP/1.1 connections at once and returns, per
// request, "<status>(<time>)" or "timeout(<time>)".
func get893(n int, url string, timeout time.Duration) []string {
	out := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cl := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
			start := time.Now()
			resp, err := cl.Get(url)
			el := time.Since(start).Round(time.Millisecond)
			if err != nil {
				out[i] = fmt.Sprintf("timeout(%v)", el)
				return
			}
			_ = resp.Body.Close()
			out[i] = fmt.Sprintf("%d(%v)", resp.StatusCode, el)
		}(i)
	}
	wg.Wait()
	return out
}

// answered893 counts the 200s in rs and lists them all, sorted.
func answered893(rs []string) (int, string) {
	ok := 0
	for _, r := range rs {
		if strings.HasPrefix(r, "200(") {
			ok++
		}
	}
	s := append([]string(nil), rs...)
	sort.Strings(s)
	return ok, fmt.Sprintf("%d/%d [%s]", ok, len(rs), strings.Join(s, " "))
}

// TestH2LockHeldAcrossAWaitingWriteDoesNotWedgeTheServer893: #906's review
// round 2. No middleware, every route sync: /hold writes its response while
// holding a mutex, /locked takes the same mutex. A slow h2c client over the
// budget GETs /hold, whose handler (on the pool, for the budget) then waits
// for window with the lock held. Eight HTTP/1.1 clients GET /locked and eight
// GET /ping. On epoll, io_uring and adaptive the /locked handlers wait for
// the lock on their event loops, and with no bound on /hold's wait they held
// them for good: no /locked and some or all /ping requests were answered,
// none after the slow client closed (the loop owning its connection could
// not see the close), and Shutdown could not stop the server. std's lockers
// waited only for /hold's write deadline. With the bound (WriteTimeout,
// 1 s here) every request must be answered, while the slow client is still
// connected and after it closes, and the server must stop (startServer761's
// cleanup checks that Start returns).
func TestH2LockHeldAcrossAWaitingWriteDoesNotWedgeTheServer893(t *testing.T) {
	const timeout = time.Second
	fill, val := body893(waitBodyLen893), body893(256<<10)
	for _, e := range engines893 {
		t.Run(e.name, func(t *testing.T) {
			var mu sync.Mutex
			addr := startServerConfig761(t, celeris.Config{Engine: e.eng, WriteTimeout: timeout}, func(s *celeris.Server) {
				s.GET("/fill", func(c *celeris.Context) error { return c.Blob(200, "application/octet-stream", fill) })
				s.GET("/hold", func(c *celeris.Context) error {
					mu.Lock()
					defer mu.Unlock()
					return c.Blob(200, "application/octet-stream", val)
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
			t.Logf("%s: WriteTimeout %v; /hold reset=%v %v; while /hold waited: /locked %s, /ping %s; after the slow client closed: /ping %s, /locked %s",
				e.name, timeout, reset, rst, sl, sb, sp, sa)
			if nl != 8 || nb != 8 {
				t.Errorf("%s: while /hold waited for window with the lock held, /locked answered %d/8 and /ping %d/8, want 8/8 each (the event loops must not wait past WriteTimeout)", e.name, nl, nb)
			}
			if np != 8 || na != 8 {
				t.Errorf("%s: after the slow client closed, /ping answered %d/8 and /locked %d/8, want 8/8 each", e.name, np, na)
			}
		})
	}
}
