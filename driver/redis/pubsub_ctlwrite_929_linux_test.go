//go:build linux

package redis

// celeris#929: a PubSub control write (SUBSCRIBE, UNSUBSCRIBE, ...) went by
// file-descriptor number to whatever socket held the number when the write
// ran. PubSub.Close, or the reconnect goroutine, could release the conn's
// number between the moment the writer read the conn and the moment it
// wrote, and the write then reached another connection that had taken the
// number: that connection's server received a command its client never sent.
//
// The tests stage the interleaving instead of racing for it. A gating
// WorkerLoop wrapper holds one control write at the entry of loop.Write, after
// the writer has read the conn and before the loop looks the number up. While
// the write is held the test runs PubSub.Close. If Close can finish (it could
// on main), the test puts a second socket B on the released number, registered
// on the same worker, and lets the write go: B's peer must receive nothing.
// If Close cannot finish while a control write is in flight, that is the
// fix: the number is not released under the write.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// writeGate pauses (or fails) one matching loop.Write.
type writeGate struct {
	mu      sync.Mutex
	armed   bool
	fail    bool
	match   func([]byte) bool
	hit     chan gateHit
	release chan struct{}
}

type gateHit struct {
	fd int
	wl engine.WorkerLoop // the real (unwrapped) loop of the conn's worker
}

func newWriteGate() *writeGate {
	return &writeGate{hit: make(chan gateHit, 1), release: make(chan struct{})}
}

func hasCmd(name string) func([]byte) bool {
	return func(b []byte) bool { return bytes.Contains(b, []byte("\r\n"+name+"\r\n")) }
}

// arm makes the next Write whose bytes match pause until release is closed.
func (g *writeGate) arm(match func([]byte) bool) {
	g.mu.Lock()
	g.armed, g.fail, g.match = true, false, match
	g.mu.Unlock()
}

// armFail makes the next Write whose bytes match return an error.
func (g *writeGate) armFail(match func([]byte) bool) {
	g.mu.Lock()
	g.armed, g.fail, g.match = true, true, match
	g.mu.Unlock()
}

var errGateInjected = errors.New("gate: injected write failure")

func (g *writeGate) before(fd int, wl engine.WorkerLoop, data []byte) error {
	g.mu.Lock()
	if !g.armed || !g.match(data) {
		g.mu.Unlock()
		return nil
	}
	g.armed = false
	fail := g.fail
	g.mu.Unlock()
	if fail {
		return errGateInjected
	}
	g.hit <- gateHit{fd: fd, wl: wl}
	<-g.release
	return nil
}

type gateLoop struct {
	engine.WorkerLoop
	g *writeGate
}

func (l *gateLoop) Write(fd int, data []byte) error {
	if err := l.g.before(fd, l.WorkerLoop, data); err != nil {
		return err
	}
	return l.WorkerLoop.Write(fd, data)
}

// gateProvider hands out gateLoops. A gateLoop does not implement the
// synchronous round-trip interfaces, so every conn it serves takes the
// asynchronous writeCommand path.
type gateProvider struct {
	engine.EventLoopProvider
	g *writeGate
}

func (p gateProvider) WorkerLoop(n int) engine.WorkerLoop {
	return &gateLoop{WorkerLoop: p.EventLoopProvider.WorkerLoop(n), g: p.g}
}

// cmdLog records the commands each server-side connection received.
type cmdLog struct {
	mu  sync.Mutex
	per map[*bufio.Writer][]string
}

func (l *cmdLog) add(w *bufio.Writer, cmd []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.per == nil {
		l.per = map[*bufio.Writer][]string{}
	}
	l.per[w] = append(l.per[w], strings.Join(cmd, " "))
}

func (l *cmdLog) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, cmds := range l.per {
		for _, c := range cmds {
			if strings.HasPrefix(c, name+" ") || c == name {
				n++
			}
		}
	}
	return n
}

type pubsubRig struct {
	fake *fakeRedis
	log  *cmdLog
	prov engine.EventLoopProvider
	gate *writeGate
	cl   *Client
}

func newPubsubRig(t *testing.T) *pubsubRig {
	t.Helper()
	b := newBroker()
	lg := &cmdLog{}
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		lg.add(w, cmd)
		b.handler(cmd, w)
	})
	b.fake = fake
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	g := newWriteGate()
	cfg := Config{Addr: fake.Addr()}
	cl := &Client{cfg: cfg, pool: newPool(cfg, gateProvider{EventLoopProvider: prov, g: g}, false)}
	t.Cleanup(func() { _ = cl.Close() })
	return &pubsubRig{fake: fake, log: lg, prov: prov, gate: g, cl: cl}
}

func (r *pubsubRig) dropServerSide() {
	r.fake.mu.Lock()
	conns := append([]net.Conn(nil), r.fake.conns...)
	r.fake.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (ps *PubSub) connNow() *redisConn {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.conn
}

// socketOnNumber puts a fresh socket (B) on number n, registered on wl, if n
// is free. It returns B's number and the number of B's peer end.
func socketOnNumber(t *testing.T, n int, wl engine.WorkerLoop) (b, peer int, ok bool) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err == nil {
		return 0, 0, false // still open: nothing released it
	}
	p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, peer = p[0], p[1]
	switch n {
	case b:
	case peer:
		b, peer = peer, b
	default:
		if err := unix.Dup2(b, n); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(b)
		b = n
	}
	_ = unix.SetNonblock(b, true)
	_ = unix.SetNonblock(peer, true)
	if err := wl.RegisterConn(b, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B, number %d): %v", b, err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(b); _ = unix.Close(b); _ = unix.Close(peer) })
	return b, peer, true
}

// closeWhileWriteHeld runs closeFn on its own goroutine while the gated write
// is held, puts B on the conn's number if closeFn released it, lets the write
// go, and fails if B's peer received anything.
func closeWhileWriteHeld(t *testing.T, g *writeGate, h gateHit, closeFn func(), writerDone <-chan error) {
	t.Helper()
	closeDone := make(chan struct{})
	go func() { closeFn(); close(closeDone) }()
	early := false
	select {
	case <-closeDone:
		early = true
	case <-time.After(300 * time.Millisecond):
	}
	var peer int
	haveB := false
	if early {
		_, peer, haveB = socketOnNumber(t, h.fd, h.wl)
	}
	close(g.release)
	select {
	case err := <-writerDone:
		t.Logf("held control write returned: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the held control write did not return within 5 s of its release")
	}
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s of the write's release")
	}
	time.Sleep(100 * time.Millisecond)
	t.Logf("929: number %d; Close finished while the write was held: %v; B on the number: %v", h.fd, early, haveB)
	if haveB {
		var buf [128]byte
		k, _ := unix.Read(peer, buf[:])
		if k > 0 {
			t.Errorf("the closed conn's control write reached another connection on number %d: its peer received %q", h.fd, buf[:k])
		}
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// (a) A caller's Subscribe races the caller's own Close.
func TestPubSubSubscribeRacingCloseKeepsToItsConn929(t *testing.T) {
	r := newPubsubRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ps, err := r.cl.newPubSub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	r.gate.arm(hasCmd("SUBSCRIBE"))
	writerDone := make(chan error, 1)
	go func() { writerDone <- ps.Subscribe(ctx, "news") }()
	var h gateHit
	select {
	case h = <-r.gate.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe never reached loop.Write")
	}
	closeWhileWriteHeld(t, r.gate, h, func() { _ = ps.Close() }, writerDone)
}

// (b) The reconnect goroutine's resubscribe races the caller's Close.
func TestPubSubReconnectResubscribeRacingCloseKeepsToItsConn929(t *testing.T) {
	r := newPubsubRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ps, err := r.cl.newPubSub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if err := ps.Subscribe(ctx, "news"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the server to see SUBSCRIBE", 5*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 1 })
	old := ps.connNow()

	r.gate.arm(hasCmd("SUBSCRIBE"))
	r.dropServerSide()
	var h gateHit
	select {
	case h = <-r.gate.hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconnect goroutine never resubscribed")
	}
	if cur := ps.connNow(); cur == old {
		t.Fatal("fixture: the held resubscribe is not on a new conn")
	}
	writerDone := make(chan error, 1)
	writerDone <- nil // the writer is the reconnect goroutine: nothing to collect
	closeWhileWriteHeld(t, r.gate, h, func() { _ = ps.Close() }, writerDone)
}

// (c) The reconnect replaces the dropped conn: the dropped conn's descriptor
// and its pubsub-pool slot are returned, and Close returns the rest.
func TestPubSubReconnectReleasesTheDroppedConn929(t *testing.T) {
	r := newPubsubRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ps, err := r.cl.newPubSub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if err := ps.Subscribe(ctx, "news"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the server to see SUBSCRIBE", 5*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 1 })
	old := ps.connNow()
	oldFd := old.fd
	oldIno := c859Ino(oldFd)
	if oldIno == 0 {
		t.Fatal("fixture: the pubsub conn's number has no inode")
	}
	if got := r.cl.pool.pubsub.Stats().Open; got != 1 {
		t.Fatalf("fixture: pubsub pool open = %d before the drop, want 1", got)
	}

	r.dropServerSide()
	waitFor(t, "the reconnect to resubscribe", 10*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 2 })
	waitFor(t, "the reconnect goroutine to finish", 5*time.Second, func() bool {
		ps.reconnectingMu.Lock()
		defer ps.reconnectingMu.Unlock()
		return !ps.reconnecting
	})
	if ps.connNow() == old {
		t.Fatal("fixture: ps.conn was not replaced")
	}
	if got := c859Ino(oldFd); got == oldIno {
		t.Errorf("the dropped conn's number %d still names its socket after the reconnect: the dropped conn was never closed", oldFd)
	}
	if got := r.cl.pool.pubsub.Stats().Open; got != 1 {
		t.Errorf("pubsub pool open = %d after the reconnect, want 1 (the dropped conn's slot was not returned)", got)
	}
	_ = ps.Close()
	if got := r.cl.pool.pubsub.Stats().Open; got != 0 {
		t.Errorf("pubsub pool open = %d after Close, want 0", got)
	}
}

// (d) A reconnect whose resubscribe write fails closes the new conn through
// the pool, once: the slot comes back, and the next attempt succeeds.
func TestPubSubReconnectFailedResubscribeReleasesItsConn929(t *testing.T) {
	r := newPubsubRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ps, err := r.cl.newPubSub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if err := ps.Subscribe(ctx, "news"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the server to see SUBSCRIBE", 5*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 1 })

	r.gate.armFail(hasCmd("SUBSCRIBE"))
	r.dropServerSide()
	waitFor(t, "the second reconnect attempt to resubscribe", 15*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 2 })
	waitFor(t, "the reconnect goroutine to finish", 5*time.Second, func() bool {
		ps.reconnectingMu.Lock()
		defer ps.reconnectingMu.Unlock()
		return !ps.reconnecting
	})
	if got := r.cl.pool.pubsub.Stats().Open; got != 1 {
		t.Errorf("pubsub pool open = %d after a failed and a successful reconnect attempt, want 1", got)
	}
	_ = ps.Close()
	if got := r.cl.pool.pubsub.Stats().Open; got != 0 {
		t.Errorf("pubsub pool open = %d after Close, want 0", got)
	}
}

// (e) The conn-level witness from the issue: a write on a closed redisConn is
// refused, and does not reach the conn that took its number.
func TestRedisWriteCommandOnClosedConnIsRefused929(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
			handleHELLO(w, 3)
			return
		}
		writeSimple(w, "OK")
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRedisConn(ctx, prov, Config{Addr: fake.Addr()}, 0)
	if err != nil {
		t.Fatalf("dialRedisConn: %v", err)
	}
	n := c.fd
	wl := prov.WorkerLoop(0)
	_ = c.Close()
	_, peer, ok := socketOnNumber(t, n, wl)
	if !ok {
		t.Fatalf("fixture: number %d is still open after Close", n)
	}
	_, werr := c.writeCommand("SUBSCRIBE", "news")
	time.Sleep(50 * time.Millisecond)
	var buf [128]byte
	k, _ := unix.Read(peer, buf[:])
	t.Logf("929: writeCommand on the closed conn returned %v; B's peer received %q", werr, buf[:max(k, 0)])
	if werr == nil {
		t.Error("writeCommand on a closed conn returned nil")
	}
	if k > 0 {
		t.Errorf("writeCommand on the closed conn reached another connection on number %d: its peer received %q", n, buf[:k])
	}
}

// (f) Stress: callers' control writes, server drops and Close interleave
// freely. Under -race this must not panic, and every conn the pubsub pool
// handed out must come back (the dropped conns, the reconnect's new conn, the
// one Close releases), exactly once.
func TestPubSubControlWritesDropsAndCloseInterleave929(t *testing.T) {
	r := newPubsubRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := 0; i < 15; i++ {
		ps, err := r.cl.newPubSub(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for k := 0; k < 20; k++ {
					_ = ps.Subscribe(ctx, "a", "b")
					_ = ps.PSubscribe(ctx, "p*")
					_ = ps.Unsubscribe(ctx, "a")
					_ = ps.SSubscribe(ctx, "s")
				}
			}(g)
		}
		wg.Add(1)
		go func() { defer wg.Done(); time.Sleep(time.Duration(i%5) * time.Millisecond); r.dropServerSide() }()
		wg.Add(1)
		go func() { defer wg.Done(); time.Sleep(time.Duration((i*3)%7) * time.Millisecond); _ = ps.Close() }()
		wg.Wait()
		_ = ps.Close()
		waitFor(t, "the reconnect goroutine to finish", 10*time.Second, func() bool {
			ps.reconnectingMu.Lock()
			defer ps.reconnectingMu.Unlock()
			return !ps.reconnecting
		})
		waitFor(t, "the pubsub pool to drain", 5*time.Second, func() bool { return r.cl.pool.pubsub.Stats().Open == 0 })
	}
}
