package redis

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// directSlabClient returns a Client whose command pool is the direct
// (net.Conn, AsyncHandlers) pool, capped at one conn so every Exec reuses it.
func directSlabClient(t *testing.T, handler func(cmd []string, w *bufio.Writer)) *Client {
	t.Helper()
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
			handleHELLO(w, 3)
			return
		}
		handler(cmd, w)
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	pool := newDirectCmdPool(Config{Addr: fake.Addr(), PoolSize: 1}, prov)
	cl := &Client{cfg: Config{Addr: fake.Addr(), PoolSize: 1}, pool: pool}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// slabCap reports cap(copySlab) of the one pooled direct conn.
func slabCap(t *testing.T, cl *Client) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.pool.releaseCmd(c)
	if !c.useDirect {
		t.Fatal("fixture: the pooled conn is not a direct conn")
	}
	return cap(c.state.copySlab)
}

// TestDirectPipelineCopySlabStaysBounded854 pins celeris#854: the direct
// (AsyncHandlers) pipeline path appended every typed reply to the per-conn
// copySlab and never reset it, so each Exec of 10 x 4 KiB values grew the
// slab by ~41 KB until it passed maxSlabRetain (1 MiB) and was dropped and
// regrown, a steady stream of MiB-sized garbage. The sync path resets the
// slab at the start of each Exec; the direct path must too, so the slab
// holds about one batch (~41 KB) however many Execs run.
func TestDirectPipelineCopySlabStaysBounded854(t *testing.T) {
	const (
		nCmds  = 10
		valLen = 4096
		nExecs = 100
		// One batch is nCmds*valLen = 40 KiB; append growth may round that
		// up. 128 KiB leaves room for that and is far below the 1 MiB the
		// unreset slab climbs to (it crosses 128 KiB on the 4th Exec).
		maxCap = 128 << 10
	)
	val := strings.Repeat("a", valLen)
	cl := directSlabClient(t, func(cmd []string, w *bufio.Writer) {
		if strings.EqualFold(cmd[0], "GET") {
			writeBulk(w, val)
			return
		}
		writeSimple(w, "OK")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var high int
	for i := 0; i < nExecs; i++ {
		p := cl.Pipeline()
		cmds := make([]*StringCmd, nCmds)
		for j := range cmds {
			cmds[j] = p.Get("demo-key")
		}
		if err := p.Exec(ctx); err != nil {
			t.Fatalf("Exec #%d: %v", i, err)
		}
		for j, c := range cmds {
			got, err := c.Result()
			if err != nil || got != val {
				t.Fatalf("Exec #%d cmd %d: len=%d err=%v, want the %d-byte value", i, j, len(got), err, valLen)
			}
		}
		p.Release()
		sc := slabCap(t, cl)
		if sc > high {
			high = sc
		}
		if sc > maxCap {
			t.Fatalf("after Exec #%d the conn's copySlab has cap %d bytes (> %d): the direct path never resets it, so it grows by one batch per Exec", i, sc, maxCap)
		}
	}
	t.Logf("copySlab high-water over %d Execs: %d bytes", nExecs, high)
}

// TestDirectPipelineKeepsFallbackResultsAcrossExecs854 guards the fix for the
// test above: a typed command whose reply has the wrong type (INCR answered
// with a bulk string) keeps its decoded Value in the Pipeline (pc.val) after
// Exec returns. That Value must not alias bytes the next Exec on the same
// conn rewrites, so resetting the slab in the direct path is only safe if
// such results are not slab-backed.
func TestDirectPipelineKeepsFallbackResultsAcrossExecs854(t *testing.T) {
	var seq int
	cl := directSlabClient(t, func(cmd []string, w *bufio.Writer) {
		if strings.EqualFold(cmd[0], "INCR") {
			seq++
			writeBulk(w, "not-a-number-"+strconv.Itoa(seq))
			return
		}
		writeBulk(w, strings.Repeat("z", 64))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First Exec: keep its Pipeline un-Released so its results stay readable.
	first := cl.Pipeline()
	// Two GETs first so the slab is sized well past the INCR's bytes and
	// later batches rewrite the same backing array instead of reallocating.
	first.Get("k")
	first.Get("k")
	first.Incr("c")
	if err := first.Exec(ctx); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if first.cmds[2].val == nil {
		t.Fatal("fixture: the mismatched INCR reply was not kept as a Value")
	}
	// Later Execs on the same (only) conn write fresh bytes over offset 0.
	for i := 0; i < 20; i++ {
		p := cl.Pipeline()
		for j := 0; j < 10; j++ {
			p.Get("k")
		}
		if err := p.Exec(ctx); err != nil {
			t.Fatalf("Exec #%d: %v", i, err)
		}
		p.Release()
	}
	if got := string(first.cmds[2].val.Str); got != "not-a-number-1" {
		t.Fatalf("a result kept from an earlier Exec was overwritten by later Execs on the same conn: got %q, want %q", got, "not-a-number-1")
	}
}
