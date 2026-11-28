//go:build linux

package epoll

// celeris#1430, part S. A loop that holds connections walked all of them every
// 25 ms (the sweep timer's tick, and the run loop's gate every 32 iterations
// once the floor has passed), whether or not any deadline was near: 42 to 71 ns
// per connection per walk, 16 % of a CPU at 50,000 idle keep-alive connections.
// The sweep now returns the earliest time at which anything it looks at could
// need action, and the loop walks again only then (or at the floor, for what the
// sweep cannot date).
//
// These tests pin the promise from both sides:
//   - the cost: the loop that holds idle connections walks them about once a
//     second, not 40 times a second (fails before the change);
//   - the accuracy: every documented timeout is met as before, on a loop that
//     has been skipping its walks, one row per state a connection can hold a
//     deadline in (passes before the change; it is the guard the change is
//     built against);
//   - the floor kept: a loop that holds an HTTP/2 connection or a detached one
//     keeps today's cadence, because their deadlines are set and moved off the
//     loop with no event for it.
//
// The walk is counted through testHookCheckTimeouts, which runs at the start of
// every checkTimeouts. The timer still ticks every 25 ms, so the wake-ups of the
// loop are unchanged (that is celeris#1430 part W); what is counted is the walk.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// probe1430 counts the walks of every loop, the walks made while a loop held a
// connection, and remembers every connection a walk met (by its remote
// address), so that a row can assert that its connection was in liveConns.
type probe1430 struct {
	mu       sync.Mutex
	live     map[*Loop]int    // walks made while the loop held a connection
	conns    map[*Loop]int    // connections held at the loop's last walk
	empties  map[*Loop]uint32 // consecutiveEmpty at the loop's last walk
	met      map[string]int   // walks that found the connection, by its remote address
	detached chan *conn.H1State
	sawDet   map[*conn.H1State]bool
}

func newProbe1430(t *testing.T) *probe1430 {
	t.Helper()
	p := &probe1430{live: map[*Loop]int{}, conns: map[*Loop]int{}, empties: map[*Loop]uint32{}, met: map[string]int{},
		detached: make(chan *conn.H1State, 4), sawDet: map[*conn.H1State]bool{}}
	h := func(l *Loop) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.conns[l] = len(l.liveConns)
		p.empties[l] = l.consecutiveEmpty
		if len(l.liveConns) == 0 {
			return
		}
		p.live[l]++
		for _, cs := range l.liveConns {
			p.met[cs.remoteAddr]++
			if h1 := cs.h1State; h1 != nil && h1.Detached.Load() && !p.sawDet[h1] {
				p.sawDet[h1] = true
				select {
				case p.detached <- h1:
				default:
				}
			}
		}
	}
	testHookCheckTimeouts.Store(&h)
	t.Cleanup(func() { testHookCheckTimeouts.Store(nil) })
	return p
}

// liveWalks is the walks loop l made while it held a connection.
func (p *probe1430) liveWalks(l *Loop) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live[l]
}

// held is the connections loop l held at its last walk.
func (p *probe1430) held(l *Loop) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[l]
}

// idleRounds is the empty rounds in a row the loop had at its last walk.
func (p *probe1430) idleRounds(l *Loop) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.empties[l]
}

// metBy is the number of walks that found the connection with remote address a.
func (p *probe1430) metBy(a string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.met[a]
}

// closeLog1430 records when the engine closed each connection, by remote address
// (OnDisconnect runs in closeConn).
type closeLog1430 struct{ at sync.Map }

func (c *closeLog1430) record(addr string) { c.at.Store(addr, time.Now()) }

// wait returns the time the engine closed the connection with remote address a.
func (c *closeLog1430) wait(t *testing.T, a string, within time.Duration) time.Time {
	t.Helper()
	for dl := time.Now().Add(within); time.Now().Before(dl); time.Sleep(2 * time.Millisecond) {
		if v, ok := c.at.Load(a); ok {
			return v.(time.Time)
		}
	}
	t.Fatalf("the connection %s was not closed within %v", a, within)
	return time.Time{}
}

// startRows1430 starts a two-loop engine with the sweep probe and the close log
// installed, and returns them.
func startRows1430(t *testing.T, async bool, h stream.Handler, mut func(*resource.Config)) (*Engine, string, *probe1430, *closeLog1430) {
	t.Helper()
	p := newProbe1430(t)
	log := &closeLog1430{}
	eng, addr := startE2E885(t, async, h, func(c *resource.Config) {
		c.OnDisconnect = log.record
		if mut != nil {
			mut(c)
		}
	})
	return eng, addr, p, log
}

// getOK1430 sends one request on c and reads the response.
func getOK1430(t *testing.T, c net.Conn, br *bufio.Reader, path string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = c.SetDeadline(time.Time{})
}

func dial1430(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// asyncHandler1430 answers every request with "ok" and routes every path to the
// connection's dispatch goroutine.
type asyncHandler1430 struct{ handler1194 }

func (asyncHandler1430) RouteAsync(_, _ string) bool { return true }
func (asyncHandler1430) HasAsyncRoutes() bool        { return true }

var _ stream.AsyncRouteResolver = asyncHandler1430{}

// TestLoopThatHoldsIdleConnectionsWalksThemAboutOncePerSecond1430 is the cost:
// 100 idle keep-alive connections, the default timeouts (nothing is due for
// seconds), 2 s. The loops that hold them made 80 walks each on main (the 25 ms
// tick); with nothing due a loop walks when the safety cap (1 s after the last
// walk) says so. The lower bound is the cap: a loop does not go without a walk
// for more than about a second.
func TestLoopThatHoldsIdleConnectionsWalksThemAboutOncePerSecond1430(t *testing.T) {
	eng, addr, p, _ := startRows1430(t, false, handler1194{big: nil}, nil)
	const n = 100
	for range n {
		c := dial1430(t, addr)
		getOK1430(t, c, bufio.NewReader(c), "/")
	}
	loops := eng.loopSnapshot()
	for dl := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		held := 0
		for _, l := range loops {
			held += p.held(l)
		}
		if held >= n {
			break
		}
		if time.Now().After(dl) {
			t.Fatalf("PREMISE: the loops held %d of the %d connections 20 s after they were opened", held, n)
		}
	}
	// Past the loops' first empty rounds, so that the window is an idle one.
	time.Sleep(500 * time.Millisecond)
	const window = 2 * time.Second
	before := map[*Loop]int{}
	for _, l := range loops {
		before[l] = p.liveWalks(l)
	}
	time.Sleep(window) // the measurement window, not a synchronisation
	holding := 0
	for _, l := range loops {
		if p.held(l) == 0 {
			continue
		}
		holding++
		walks := p.liveWalks(l) - before[l]
		t.Logf("celeris1430: loop %d held %d idle connections and walked them %d times in %v (the 25 ms tick gives %d)",
			l.id, p.held(l), walks, window, int(window/sweepFloor))
		if walks > 6 {
			t.Errorf("loop %d walked its %d idle connections %d times in %v, want at most 6: with nothing due the walk is made about once a second (the safety cap), not at every tick",
				l.id, p.held(l), walks, window)
		}
		if walks < 1 {
			t.Errorf("loop %d did not walk its connections once in %v: the safety cap (the next walk at most about a second after the last) is gone", l.id, window)
		}
	}
	if holding == 0 {
		t.Fatal("PREMISE: no loop holds a connection")
	}
}

// row1430 is one state a connection can hold a deadline in: the engine's
// configuration, the connection's story, and the window the close must fall in
// (measured from t0, which the story returns).
type row1430 struct {
	name     string
	async    bool
	h        stream.Handler
	mut      func(*resource.Config)
	drive    func(t *testing.T, addr string) (c net.Conn, t0 time.Time)
	earliest time.Duration
	latest   time.Duration
}

// TestDeadlinesAreMetOnALoopThatSkipsItsWalks1430: each row has one connection in
// the state, on a loop that holds nothing else and is past its first 100 empty
// rounds before the state is entered, so that whatever the loop does to wait
// longer is engaged. The row asserts that the connection is met by a walk (it is
// in liveConns) and closed within [earliest-50 ms, latest+200 ms]: the bounds of
// celeris#1194. The rows pass before the change; they are what keeps the
// deadline source of the new walk honest.
func TestDeadlinesAreMetOnALoopThatSkipsItsWalks1430(t *testing.T) {
	big := make([]byte, 32<<20)
	h := handler1194{big: big}
	quiet := func(c *resource.Config) { c.ReadHeaderTimeout = -1 }
	rows := []row1430{
		{
			name: "http1 idle keep-alive (IdleTimeout 1 s)", h: h,
			mut: func(c *resource.Config) { quiet(c); c.IdleTimeout = time.Second },
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				getOK1430(t, c, bufio.NewReader(c), "/")
				return c, time.Now()
			},
			earliest: time.Second, latest: time.Second,
		},
		{
			name: "http1 stalled body (ReadTimeout 1 s)", h: h,
			mut: func(c *resource.Config) { quiet(c); c.ReadTimeout = time.Second },
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				if _, err := fmt.Fprint(c, "POST /up HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"+string(make([]byte, 100))); err != nil {
					t.Fatal(err)
				}
				return c, time.Now()
			},
			earliest: time.Second, latest: time.Second,
		},
		{
			name: "http1 headers that stop (ReadHeaderTimeout 1 s)", h: h,
			mut: func(c *resource.Config) { c.ReadHeaderTimeout = time.Second },
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				t0 := time.Now()
				if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n"); err != nil {
					t.Fatal(err)
				}
				return c, t0
			},
			earliest: time.Second, latest: time.Second,
		},
		{
			name: "protocol not decided (ReadHeaderTimeout 1 s, Auto)", h: h,
			mut: func(c *resource.Config) {
				c.ReadHeaderTimeout = time.Second
				c.Protocol = engine.Auto
				c.DisableDeferAccept = true // the budget runs from the accept, not from the first byte
			},
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				return c, time.Now()
			},
			earliest: time.Second, latest: time.Second,
		},
		{
			name: "async keep-alive, headers that stop, armed by the dispatch goroutine (ReadHeaderTimeout 1 s)", async: true, h: asyncHandler1430{h},
			mut: func(c *resource.Config) { c.ReadHeaderTimeout = time.Second },
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				getOK1430(t, c, bufio.NewReader(c), "/a")
				// The deadline was cleared with the request; the next request's
				// first bytes arm it again, on the dispatch goroutine.
				t0 := time.Now()
				if _, err := fmt.Fprint(c, "GET /a HTTP/1.1\r\nHost: x\r\n"); err != nil {
					t.Fatal(err)
				}
				return c, t0
			},
			earliest: time.Second, latest: time.Second,
		},
		{
			name: "response parked, peer takes nothing (WriteTimeout 1 s)", h: h,
			mut: func(c *resource.Config) {
				quiet(c)
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 16 << 10
			},
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				t0 := time.Now()
				if _, err := fmt.Fprint(c, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				return c, t0
			},
			earliest: time.Second, latest: 2*time.Second + 100*time.Millisecond,
		},
		{
			name: "closing drain, peer takes nothing (WriteTimeout 1 s)", h: h,
			mut: func(c *resource.Config) {
				quiet(c)
				c.WriteTimeout = time.Second
				c.Resources.SocketSend = 16 << 10
			},
			drive: func(t *testing.T, addr string) (net.Conn, time.Time) {
				c := dial1430(t, addr)
				t0 := time.Now()
				if _, err := fmt.Fprint(c, "GET /big HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				return c, t0
			},
			earliest: 500 * time.Millisecond, latest: 2 * time.Second,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			eng, addr, p, log := startRows1430(t, r.async, r.h, r.mut)
			loops := eng.loopSnapshot()
			// Every loop past 100 empty rounds: the wait is at its longest tier.
			for dl := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				ok := true
				for _, l := range loops {
					if p.idleRounds(l) <= 100 {
						ok = false
					}
				}
				if ok {
					break
				}
				if time.Now().After(dl) {
					t.Fatal("PREMISE: the loops did not reach 100 empty rounds in 30 s")
				}
			}
			c, t0 := r.drive(t, addr)
			a := c.LocalAddr().String()
			at := log.wait(t, a, r.latest+15*time.Second)
			got := at.Sub(t0)
			t.Logf("celeris1430 %s: closed after %v (window %v to %v)", r.name, got.Round(time.Millisecond), r.earliest, r.latest)
			if got < r.earliest-slack1194 {
				t.Errorf("closed after %v, want no sooner than %v: a timeout that fired early", got.Round(time.Millisecond), r.earliest-slack1194)
			}
			if got > r.latest+lateness1194 {
				t.Errorf("closed after %v, want within %v of %v: a deadline the loop did not walk for in time", got.Round(time.Millisecond), lateness1194, r.latest)
			}
			if n := p.metBy(a); n == 0 {
				t.Errorf("no walk found the connection in liveConns: it was closed by something that is not the sweep, or is not in the set the sweep walks")
			}
		})
	}
}

// TestConnectionThatArrivesOnALoopThatSkipsIsOnTime1430: a connection that keeps
// a loop's earliest deadline a second away (a ping every 250 ms, ReadTimeout 1
// s) is not what the next walk is waiting for once a newcomer comes in: six
// connections, which land on both loops, arrive after the loops have skipped
// walks for a while and each idles out at ReadTimeout after its request.
func TestConnectionThatArrivesOnALoopThatSkipsIsOnTime1430(t *testing.T) {
	_, addr, p, log := startRows1430(t, false, handler1194{big: nil}, func(c *resource.Config) {
		c.ReadHeaderTimeout = -1
		c.ReadTimeout = time.Second
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() { close(stop); wg.Wait() })
	var pings atomic.Int64
	for range 4 {
		c := dial1430(t, addr)
		br := bufio.NewReader(c)
		getOK1430(t, c, br, "/")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(250 * time.Millisecond):
				}
				if _, err := fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					return
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				pings.Add(1)
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond) // the loops walk at their own pace meanwhile
	type probe struct {
		addr string
		t0   time.Time
	}
	var probes []probe
	for range 6 {
		c := dial1430(t, addr)
		getOK1430(t, c, bufio.NewReader(c), "/")
		probes = append(probes, probe{c.LocalAddr().String(), time.Now()})
		time.Sleep(70 * time.Millisecond)
	}
	for _, pr := range probes {
		at := log.wait(t, pr.addr, 20*time.Second)
		got := at.Sub(pr.t0)
		if got < time.Second-slack1194 || got > time.Second+lateness1194 {
			t.Errorf("the connection that arrived at %v after the pings began closed after %v, want within [%v, %v]",
				pr.t0.Format("15:04:05.000"), got.Round(time.Millisecond), time.Second-slack1194, time.Second+lateness1194)
		}
		if p.metBy(pr.addr) == 0 {
			t.Errorf("no walk found %s in liveConns", pr.addr)
		}
	}
	if pings.Load() == 0 {
		t.Error("PREMISE: the keep-alive connections never pinged")
	}
}

// TestLoopWithAnHTTP2ConnectionKeepsTheFloor1430: an HTTP/2 connection's request,
// write and header-block deadlines are set and moved off the loop's events
// (the stream manager's arm sites, a pool goroutine's write hint), so a loop
// that holds one is not skipped: it walks at the floor, as it always did. At
// least 25 walks in 1 s on the loop that holds the connection (the tick gives
// 40). Passes before the change.
func TestLoopWithAnHTTP2ConnectionKeepsTheFloor1430(t *testing.T) {
	eng, addr, p, _ := startRows1430(t, false, handler1194{big: nil}, func(c *resource.Config) { c.Protocol = engine.H2C })
	c := dial1430(t, addr)
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"); err != nil {
		t.Fatal(err)
	}
	loops := eng.loopSnapshot()
	var holder *Loop
	for dl := time.Now().Add(10 * time.Second); holder == nil; time.Sleep(5 * time.Millisecond) {
		for _, l := range loops {
			if p.held(l) > 0 {
				holder = l
			}
		}
		if time.Now().After(dl) {
			t.Fatal("PREMISE: no loop holds the HTTP/2 connection")
		}
	}
	time.Sleep(500 * time.Millisecond)
	before := p.liveWalks(holder)
	time.Sleep(time.Second) // the measurement window, not a synchronisation
	walks := p.liveWalks(holder) - before
	t.Logf("celeris1430: %d walks in 1 s by the loop that holds an HTTP/2 connection (the 25 ms tick gives 40)", walks)
	if walks < 25 {
		t.Errorf("the loop that holds an HTTP/2 connection walked %d times in 1 s, want at least 25: its deadlines have no event on the loop, so it keeps the floor", walks)
	}
}

// TestLoopWithADetachedConnectionKeepsTheFloor1430: a detached connection's idle
// deadline (SetWSIdleDeadline, a WebSocket's, an SSE stream's end) is stored from
// the middleware's goroutine with no event for the loop. The loop that holds one
// walks at the floor, and a deadline stored 200 ms ahead while the loop is idle
// is acted on within the tick that follows it. Passes before the change.
func TestLoopWithADetachedConnectionKeepsTheFloor1430(t *testing.T) {
	eng, addr, p, log := startRows1430(t, true, floorHandler885{}, func(c *resource.Config) { c.ReadHeaderTimeout = -1 })
	dc := dial1430(t, addr)
	if _, err := dc.Write([]byte("GET /detach HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	var h1 *conn.H1State
	select {
	case h1 = <-p.detached:
	case <-time.After(10 * time.Second):
		t.Fatal("PREMISE: no walk met the detached connection within 10 s")
	}
	var holder *Loop
	for _, l := range eng.loopSnapshot() {
		if p.held(l) > 0 {
			holder = l
		}
	}
	if holder == nil {
		t.Fatal("PREMISE: no loop holds the detached connection")
	}
	time.Sleep(500 * time.Millisecond)
	before := p.liveWalks(holder)
	time.Sleep(time.Second) // the measurement window, not a synchronisation
	walks := p.liveWalks(holder) - before
	t.Logf("celeris1430: %d walks in 1 s by the loop that holds a detached connection (the 25 ms tick gives 40)", walks)
	if walks < 25 {
		t.Errorf("the loop that holds a detached connection walked %d times in 1 s, want at least 25: its idle deadline is stored off the loop, so it keeps the floor", walks)
	}
	const lead = 200 * time.Millisecond
	deadline := time.Now().Add(lead)
	h1.IdleDeadlineNs.Store(mono.FromTime(deadline))
	at := log.wait(t, dc.LocalAddr().String(), 10*time.Second)
	if late := at.Sub(deadline); late > lead+100*time.Millisecond {
		t.Errorf("the detached connection was reaped %v after its idle deadline, want within %v", late.Round(time.Millisecond), lead+100*time.Millisecond)
	}
}
