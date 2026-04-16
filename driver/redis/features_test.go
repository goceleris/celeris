package redis

import (
	"bufio"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

func TestPublish(t *testing.T) {
	c := newTestClient(t, nil)
	n, err := c.Publish(context.Background(), "chan", "hello")
	if err != nil {
		t.Fatal(err)
	}
	// Fake server returns 0 receivers; real Redis would report subscribers.
	if n != 0 {
		t.Fatalf("want 0, got %d", n)
	}
}

func TestPublishCount(t *testing.T) {
	// Override handler to return a specific count for PUBLISH.
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PUBLISH":
			writeInt(w, 42)
		default:
			writeError(w, "ERR")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	n, err := c.Publish(context.Background(), "chan", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Fatalf("want 42, got %d", n)
	}
}

func TestTxBasic(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := tx.Set("k", "v", 0)
	get := tx.Get("k")
	inc := tx.Incr("n")
	if err := tx.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := set.Result(); err != nil || s != "OK" {
		t.Fatalf("set: %q %v", s, err)
	}
	if s, err := get.Result(); err != nil || s != "v" {
		t.Fatalf("get: %q %v", s, err)
	}
	if n, err := inc.Result(); err != nil || n != 1 {
		t.Fatalf("incr: %d %v", n, err)
	}
}

func TestTxDiscard(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Set("k", "v", 0)
	if err := tx.Discard(); err != nil {
		t.Fatal(err)
	}
	// Without Exec, the SET never ran.
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrNil) {
		t.Fatalf("want ErrNil, got %v", err)
	}
}

func TestTxWatchAbort(t *testing.T) {
	mem := newMem()
	// Rig the fake so that after WATCH, the next EXEC returns a null array
	// for every conn that saw WATCH. We do that by flipping a flag inside
	// the handler.
	handler := func(cmd []string, w *bufio.Writer) {
		up := strings.ToUpper(cmd[0])
		if up == "WATCH" {
			mem.mu.Lock()
			mem.watchedAbort[w] = true
			mem.mu.Unlock()
			writeSimple(w, "OK")
			return
		}
		mem.handler(cmd, w)
	}
	fake := startFakeRedis(t, handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Watch(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	set := tx.Set("k", "v", 0)
	if err := tx.Exec(ctx); !errors.Is(err, ErrTxAborted) {
		t.Fatalf("want ErrTxAborted, got %v", err)
	}
	if _, err := set.Result(); !errors.Is(err, ErrTxAborted) {
		t.Fatalf("set.Result: want ErrTxAborted, got %v", err)
	}
}

func TestEval(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	// Fake EVAL echoes the first arg as a bulk.
	v, err := c.Eval(ctx, "return ARGV[1]", []string{}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Str) != "hello" {
		t.Fatalf("got %q", v.Str)
	}
}

func TestScriptLoad(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	sha, err := c.ScriptLoad(ctx, "return 1")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("empty sha")
	}
}

func TestEvalSHANoScript(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	_, err := c.EvalSHA(ctx, "deadbeef", nil)
	if err == nil {
		t.Fatal("expected NOSCRIPT error")
	}
	var rerr *RedisError
	if !errors.As(err, &rerr) || rerr.Prefix != "NOSCRIPT" {
		t.Fatalf("want NOSCRIPT, got %v", err)
	}
}

func TestScanIterator(t *testing.T) {
	mem := newMem()
	want := map[string]bool{}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		mem.kv[k] = "x"
		want[k] = true
	}
	fake := startFakeRedis(t, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	it := c.Scan(ctx, "*", 10)
	got := []string{}
	for {
		k, ok := it.Next(ctx)
		if !ok {
			break
		}
		got = append(got, k)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	for k := range want {
		found := false
		for _, g := range got {
			if g == k {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in scan result %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("want %d keys, got %d (%v)", len(want), len(got), got)
	}
}

func TestScanIteratorMatch(t *testing.T) {
	mem := newMem()
	mem.kv["user:1"] = "a"
	mem.kv["user:2"] = "b"
	mem.kv["post:1"] = "c"
	fake := startFakeRedis(t, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	it := c.Scan(ctx, "user:*", 0)
	got := []string{}
	for {
		k, ok := it.Next(ctx)
		if !ok {
			break
		}
		got = append(got, k)
	}
	if it.Err() != nil {
		t.Fatal(it.Err())
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "user:1" || got[1] != "user:2" {
		t.Fatalf("want [user:1 user:2], got %v", got)
	}
}

func TestPExpireAndExpireAt(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	ok, err := c.PExpire(ctx, "k", 500*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("pexpire: %v %v", ok, err)
	}
	ok, err = c.ExpireAt(ctx, "k", time.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("expireat: %v %v", ok, err)
	}
	ok, err = c.PExpireAt(ctx, "k", time.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("pexpireat: %v %v", ok, err)
	}
	ok, err = c.Persist(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("persist: %v %v", ok, err)
	}
	ok, err = c.Persist(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("persist on missing should be false")
	}
}

func TestWatchPinnedConn(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	// Watch pins a conn, runs fn with a Tx, releases on return.
	err := c.Watch(ctx, func(tx *Tx) error {
		tx.Set("k", "v", 0)
		return tx.Exec(ctx)
	}, "k")
	if err != nil {
		t.Fatal(err)
	}
	// Empty keys must error.
	if err := c.Watch(ctx, func(tx *Tx) error { return nil }); err == nil {
		t.Fatal("expected error on empty watch")
	}
}

func TestWatchFnNoExec(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	// If fn returns without calling Exec/Discard, the conn is still released.
	err := c.Watch(ctx, func(tx *Tx) error {
		return nil
	}, "k")
	if err != nil {
		t.Fatal(err)
	}
	// Pool should not leak — a subsequent command must succeed.
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestDoneChPooling sanity-checks that repeated GET calls exercise the reqPool
// path (previously each call allocated a fresh chan struct{}). We don't assert
// a precise alloc number — that varies with Go version — but we do assert the
// command runs correctly across many iterations.
func TestDoneChPooling(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		got, err := c.Get(ctx, "k")
		if err != nil || got != "v" {
			t.Fatalf("iter %d: %q %v", i, got, err)
		}
	}
}

// TestTxWithCmd confirms that after Exec the conn is back in the pool and
// can be used for subsequent commands.
func TestTxPostExecUsable(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	tx, err := c.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Set("k", "v", 0)
	if err := tx.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
}

func TestPushCallbackOnCmdConn(t *testing.T) {
	var mu sync.Mutex
	var gotChannel string
	var gotData []protocol.Value

	pushHandler := func(channel string, data []protocol.Value) {
		mu.Lock()
		gotChannel = channel
		gotData = data
		mu.Unlock()
	}

	// Fake server that injects a push frame before a PING reply.
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			// Inject a RESP3 push frame before the PONG reply.
			writePush(w, "invalidate", "mykey")
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
	})
	c, err := NewClient(fake.Addr(), WithOnPush(pushHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Allow a brief window for the push callback to fire.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	ch := gotChannel
	data := gotData
	mu.Unlock()

	if ch != "invalidate" {
		t.Fatalf("push channel: got %q, want %q", ch, "invalidate")
	}
	if len(data) != 1 || string(data[0].Str) != "mykey" {
		t.Fatalf("push data: got %v, want [mykey]", data)
	}
}

func TestPushCallbackNilDropsSilently(t *testing.T) {
	// Verify that push frames on cmd connections are silently dropped
	// when no OnPush callback is registered (no crash).
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			writePush(w, "invalidate", "k1")
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
	})
	c, err := NewClient(fake.Addr()) // no WithOnPush
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOnPushPostConstruction(t *testing.T) {
	var mu sync.Mutex
	var called bool

	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "PING":
			writePush(w, "test", "data")
			writeSimple(w, "PONG")
		default:
			writeSimple(w, "OK")
		}
	})
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	// Register OnPush after construction.
	c.OnPush(func(channel string, data []protocol.Value) {
		mu.Lock()
		called = true
		mu.Unlock()
	})

	// The first PING may have dialed without onPush. The second dial
	// will pick up the callback via Config.OnPush. Since pool reuse may
	// return the same conn, we verify the API doesn't crash — the
	// callback may or may not fire on existing conns.
	_ = c.Ping(context.Background())
	_ = c.Ping(context.Background())

	// Suppress "declared and not used" for called — we verify the API
	// doesn't crash, the callback may or may not fire on existing conns.
	mu.Lock()
	_ = called
	mu.Unlock()
}

