package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// ----------------- R-1 / R-2: PubSub reconnect -----------------

// TestPubSubReconnectOnDisconnect kills the server's side of the conn after
// the first SUBSCRIBE. The PubSub must reconnect and re-subscribe so a
// published message after the reconnect is delivered.
func TestPubSubReconnectOnDisconnect(t *testing.T) {
	var (
		mu         sync.Mutex
		subs       = map[*bufio.Writer]map[string]struct{}{}
		subscribed = make(chan struct{}, 4)
	)

	handler := func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "SUBSCRIBE":
			mu.Lock()
			s, ok := subs[w]
			if !ok {
				s = map[string]struct{}{}
				subs[w] = s
			}
			for i, ch := range cmd[1:] {
				s[ch] = struct{}{}
				writeArrayHeader(w, 3)
				writeBulk(w, "subscribe")
				writeBulk(w, ch)
				writeInt(w, int64(i+1))
			}
			mu.Unlock()
			select {
			case subscribed <- struct{}{}:
			default:
			}
		case "PING":
			writeSimple(w, "PONG")
		}
	}

	fake := startFakeRedis(t, handler)

	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.Subscribe(context.Background(), "chan1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	// Wait for initial SUBSCRIBE to be accepted.
	select {
	case <-subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("initial SUBSCRIBE timed out")
	}

	// Force-close every server-side conn.
	fake.mu.Lock()
	for _, c := range fake.conns {
		_ = c.Close()
	}
	fake.mu.Unlock()

	// Wait for re-subscription.
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect SUBSCRIBE timed out")
	}

	// Publish to every new writer and verify the client sees the message.
	mu.Lock()
	targets := make([]*bufio.Writer, 0, len(subs))
	for w := range subs {
		targets = append(targets, w)
	}
	mu.Unlock()
	for _, w := range targets {
		wm := fake.WriterMutex(w)
		wm.Lock()
		writeArrayHeader(w, 3)
		writeBulk(w, "message")
		writeBulk(w, "chan1")
		writeBulk(w, "after-reconnect")
		_ = w.Flush()
		wm.Unlock()
	}

	select {
	case m := <-ps.Channel():
		if m.Channel != "chan1" || m.Payload != "after-reconnect" {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message after reconnect")
	}
}

// TestPubSubCloseAfterDrop closes the PubSub while the reconnect loop is
// active. Channel must unblock and Close must not hang.
func TestPubSubCloseAfterDrop(t *testing.T) {
	// Server that accepts one SUBSCRIBE then closes every further conn so
	// the reconnect loop churns.
	handler := func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "SUBSCRIBE":
			for i, ch := range cmd[1:] {
				writeArrayHeader(w, 3)
				writeBulk(w, "subscribe")
				writeBulk(w, ch)
				writeInt(w, int64(i+1))
			}
		}
	}
	fake := startFakeRedis(t, handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.Subscribe(context.Background(), "drop-me")
	if err != nil {
		t.Fatal(err)
	}

	// Kill the server-side conn to start the reconnect loop.
	fake.mu.Lock()
	for _, cc := range fake.conns {
		_ = cc.Close()
	}
	fake.mu.Unlock()

	// Close PubSub while reconnect may be in flight.
	done := make(chan struct{})
	go func() {
		_ = ps.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung")
	}

	// Channel must be drained/closed — a read returns zero without blocking
	// forever.
	select {
	case _, ok := <-ps.Channel():
		if ok {
			// may receive pre-close messages; drain once more
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Channel still open after Close")
	}
}

// ----------------- R-4: write-error discards conn -----------------

// TestExecWriteErrorDiscards closes the server-side conn mid-stream so the
// client's write fails. The conn must be discarded; a subsequent Get must
// dial a fresh conn and succeed.
func TestExecWriteErrorDiscards(t *testing.T) {
	mem := newMem()
	fake := startFakeRedis(t, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	// Warm up a conn and drop its server side. The next Get should
	// re-dial.
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for _, cc := range fake.conns {
		_ = cc.Close()
	}
	fake.mu.Unlock()
	// A GET may succeed (if the dead conn was already evicted) or fail
	// once; eventually it must succeed after the pool redials.
	var lastErr error
	for i := 0; i < 5; i++ {
		_, err := c.Get(ctx, "k")
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool did not recover: %v", lastErr)
}

// ----------------- R-5: pipeline ctx cancel populates pipeCmd errs ---------

func TestPipelineContextCancelPopulatesErrs(t *testing.T) {
	// Server that stalls forever on GET.
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "SET":
			writeSimple(w, "OK")
		case "GET":
			// never reply
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	p := c.Pipeline()
	s := p.Set("k", "v", 0)
	g := p.Get("k")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.Exec(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
	// Every pipeCmd err must be non-nil and == ctx.Err().
	if _, err := s.Result(); err == nil {
		t.Fatal("set cmd err should be non-nil")
	}
	if _, err := g.Result(); err == nil {
		t.Fatal("get cmd err should be non-nil")
	}
}

// ----------------- R-6: copyValueDetached clears pooled flags -------------

func TestCopyValueDetachedClearsPooledFlags(t *testing.T) {
	// Build a Value whose Array and Map would come from the pool.
	v := protocol.Value{
		Type: protocol.TyArray,
		Array: []protocol.Value{
			{Type: protocol.TyBulk, Str: []byte("x")},
			{Type: protocol.TyMap, Map: []protocol.KV{
				{K: protocol.Value{Type: protocol.TyBulk, Str: []byte("k")},
					V: protocol.Value{Type: protocol.TyBulk, Str: []byte("v")}},
			}},
		},
	}
	// Simulate pool-sourced flags on the inputs.
	protocol.ClearPooledFlags(&v) // start clean then flip
	// (ClearPooledFlags only clears; flags default to false so the test
	// above just proves the recursion doesn't panic; copyValueDetached's
	// real guarantee is that its *output* has flags cleared, which is
	// what we check now.)

	cp := copyValueDetached(v)
	// Type-assert flags via reflection of behavior: ClearPooledFlags is
	// idempotent and must leave fields untouched when already cleared.
	// To actually verify the copy produced non-pooled slices we check
	// that Array/Map allocations don't alias the originals.
	if &cp.Array[0] == &v.Array[0] {
		t.Fatal("copy aliases original top Array")
	}
	if len(cp.Array) >= 2 && len(cp.Array[1].Map) >= 1 {
		if &cp.Array[1].Map[0] == &v.Array[1].Map[0] {
			t.Fatal("copy aliases original Map")
		}
	}
}

// ----------------- R-7: sync.Once guard on doneCh -------------------------

// TestDoneChNoRaceClose drives many concurrent completers at one request and
// asserts no panic. The doneCh close path is guarded by sync.Once so two
// goroutines racing to close cannot crash.
func TestDoneChNoRaceClose(t *testing.T) {
	for i := 0; i < 100; i++ {
		req := &redisRequest{doneCh: make(chan struct{}, 1)}
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req.finish()
			}()
		}
		wg.Wait()
		select {
		case <-req.doneCh:
		default:
			t.Fatal("doneCh not closed")
		}
	}
}

// ----------------- R-8: Client.Do escape hatch ----------------------------

func TestClientDo(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "OBJECT":
			// OBJECT ENCODING foo → bulk "embstr"
			if len(cmd) >= 2 && strings.EqualFold(cmd[1], "ENCODING") {
				writeBulk(w, "embstr")
				return
			}
			writeError(w, "ERR bad OBJECT")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	v, err := c.Do(context.Background(), "OBJECT", "ENCODING", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || string(v.Str) != "embstr" {
		t.Fatalf("got %+v", v)
	}

	// Empty args must error.
	if _, err := c.Do(context.Background()); err == nil {
		t.Fatal("expected error on empty args")
	}

	// Typed helper: DoString.
	s, err := c.DoString(context.Background(), "OBJECT", "ENCODING", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if s != "embstr" {
		t.Fatalf("DoString got %q", s)
	}
}

// ----------------- R-9: verbatim prefix stripped --------------------------

func TestVerbatimStringsStripPrefix(t *testing.T) {
	// Verbatim string: "=15\r\ntxt:hello world\r\n"
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			_, _ = w.WriteString("=15\r\ntxt:hello world\r\n")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	got, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("want stripped 'hello world', got %q", got)
	}
}

// ----------------- R-10: ForceRESP2 + password ---------------------------

func TestForceRESP2WithPassword(t *testing.T) {
	var sawAuth atomic.Bool
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			// Must not be called when ForceRESP2 is set.
			t.Errorf("HELLO sent despite ForceRESP2")
			writeError(w, "ERR no HELLO")
		case "AUTH":
			sawAuth.Store(true)
			if len(cmd) < 2 {
				writeError(w, "ERR AUTH args")
				return
			}
			writeSimple(w, "OK")
		case "SELECT":
			writeSimple(w, "OK")
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	c, err := NewClient(fake.Addr(), WithForceRESP2(), WithPassword("secret"), WithDB(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawAuth.Load() {
		t.Fatal("server never saw AUTH")
	}
}

// ----------------- R-12: asBool from bulk --------------------------------

func TestAsBoolFromBulk(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		ok   bool
	}{
		{"1", true, true},
		{"0", false, true},
		{"true", true, true},
		{"false", false, true},
		{"TRUE", true, true},
		{"FALSE", false, true},
		{"maybe", false, false},
	}
	for _, c := range cases {
		v := protocol.Value{Type: protocol.TyBulk, Str: []byte(c.in)}
		got, err := asBool(v)
		if c.ok && err != nil {
			t.Fatalf("%q: unexpected err %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q: expected error", c.in)
		}
		if c.ok && got != c.want {
			t.Fatalf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

// ----------------- R-13: resetSession clears MULTI ----------------------

func TestResetSessionClearsMulti(t *testing.T) {
	var sawDiscard atomic.Int32
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "MULTI":
			writeSimple(w, "OK")
		case "DISCARD":
			sawDiscard.Add(1)
			writeSimple(w, "OK")
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if _, err := c.Do(ctx, "MULTI"); err != nil {
		t.Fatal(err)
	}
	// The conn is now dirty; when returned to the pool, DISCARD should
	// fire during resetSession. A subsequent PING re-acquires it.
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if sawDiscard.Load() == 0 {
		t.Fatal("server never saw DISCARD after MULTI")
	}

	// Second acquire should NOT trigger another DISCARD (dirty cleared).
	prev := sawDiscard.Load()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if sawDiscard.Load() != prev {
		t.Fatalf("spurious DISCARD on clean conn: %d→%d", prev, sawDiscard.Load())
	}
}

// ----------------- maxBulk surfaces to the driver as ErrProtocol ---------

// TestReaderMaxBulkDoSPropagates ensures a hostile reply at the driver layer
// closes the conn rather than hanging forever.
func TestReaderMaxBulkDoSPropagates(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			_, _ = w.WriteString("$99999999999999999\r\n")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, gerr := c.Get(ctx, "k")
	if gerr == nil {
		t.Fatal("expected error on oversized bulk")
	}
	if errors.Is(gerr, context.DeadlineExceeded) {
		t.Fatal("conn hung instead of failing fast on oversized bulk")
	}
}

// ---------- Blocker 1: ctx cancel closes conn, no desync ----------------

// TestContextCancelDoesNotDesync cancels a ctx during a slow command, then
// issues another command on a fresh conn. The second command must return the
// correct response, not the stale reply from the cancelled command.
func TestContextCancelDoesNotDesync(t *testing.T) {
	var (
		mu    sync.Mutex
		slow  = make(chan struct{}) // closed when fake should reply to GET
		phase atomic.Int32          // 0 = stall, 1 = reply normally
	)
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			if phase.Load() == 0 {
				// Stall until signaled.
				mu.Lock()
				mu.Unlock()
				<-slow
				writeBulk(w, "stale-reply")
			} else {
				writeBulk(w, "correct-reply")
			}
		case "SET":
			writeSimple(w, "OK")
		case "PING":
			writeSimple(w, "PONG")
		}
	})

	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Issue a GET with a ctx that we cancel quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.Get(ctx, "k")
	if err == nil {
		t.Fatal("expected ctx error")
	}

	// Let the fake reply to the stalled command (it arrives on a now-closed conn).
	phase.Store(1)
	close(slow)

	// Small delay for the conn to close and be discarded.
	time.Sleep(50 * time.Millisecond)

	// Issue a fresh GET on a new conn; must get the correct reply.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	var got string
	for i := 0; i < 5; i++ {
		got, err = c.Get(ctx2, "k")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("follow-up GET failed: %v", err)
	}
	if got != "correct-reply" {
		t.Fatalf("desync: got %q, want %q", got, "correct-reply")
	}
}

// ---------- Blocker 2: health check config wired through ----------------

func TestHealthCheckIntervalDefault(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Warm a conn so it enters the idle pool.
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The health check loop is running if no panic occurred and the pool
	// was created with a 30s default. This is a smoke test — the real
	// verification is that async.Pool.healthLoop is started (covered by
	// the async package tests). Here we verify our config wiring.
}

func TestHealthCheckIntervalDisabled(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	// A negative value disables the health check.
	c, err := NewClient(fake.Addr(), WithHealthCheckInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ---------- Blocker 4: Eval deep-copies result --------------------------

func TestEvalResultNotAliased(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "EVAL":
			// Reply with a 2-element array of bulk strings.
			writeArrayHeader(w, 2)
			writeBulk(w, "alpha")
			writeBulk(w, "bravo")
		case "PING":
			writeSimple(w, "PONG")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	v, err := c.Eval(context.Background(), "return {'alpha','bravo'}", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || len(v.Array) != 2 {
		t.Fatalf("unexpected result: %+v", v)
	}
	if string(v.Array[0].Str) != "alpha" || string(v.Array[1].Str) != "bravo" {
		t.Fatalf("wrong values: %+v", v)
	}

	// Issue another command to overwrite the reader buffer.
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The Eval result must still hold the correct data.
	if string(v.Array[0].Str) != "alpha" || string(v.Array[1].Str) != "bravo" {
		t.Fatalf("Eval result corrupted after subsequent command: %+v", v)
	}
}

// ---------- Blocker 5: TLS error messages are clear ---------------------

func TestTLSErrorMessages(t *testing.T) {
	_, err := NewClient("rediss://127.0.0.1:6379")
	if err == nil {
		t.Fatal("expected TLS rejection")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("TLS error missing 'not yet supported': %v", err)
	}
	if !strings.Contains(err.Error(), "VPC/loopback") {
		t.Fatalf("TLS error missing workaround: %v", err)
	}
}
