//go:build linux

package redis

// celeris#1265: on the sync path (conn.syncMulti != nil, Linux standalone
// epoll loop) execManyCore resets copySlab to [:0] under writerMu and
// dispatch routes untyped replies through copyValueSlab, so the Values a
// caller keeps (Tx results, Pipeline fallback Values) are views into the
// slab. The conn goes back to the pool before the caller reads them; the next
// Exec on that conn rewrites the slab from offset 0. These tests keep a
// result across a later Exec on the same conn and check its bytes.

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syncSlabFake struct {
	fill  atomic.Int32 // byte the next 4 KiB values are made of
	mu    sync.Mutex
	inTx  bool
	queue []string // keys of the GETs queued inside MULTI
}

func (f *syncSlabFake) val() string { return strings.Repeat(string(rune(f.fill.Load())), 4096) }

func (f *syncSlabFake) handle(cmd []string, w *bufio.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.ToUpper(cmd[0]) {
	case "HELLO":
		handleHELLO(w, 3)
	case "MULTI":
		f.inTx, f.queue = true, nil
		writeSimple(w, "OK")
	case "EXEC":
		f.inTx = false
		writeArrayHeader(w, len(f.queue))
		for _, key := range f.queue {
			switch key {
			case "arr": // a nested array of 4 KiB strings
				writeArrayHeader(w, 3)
				for i := 0; i < 3; i++ {
					writeBulk(w, f.val())
				}
			case "map": // a nested RESP3 map of 4 KiB keys and values
				_, _ = w.WriteString("%2\r\n")
				for i := 0; i < 4; i++ {
					writeBulk(w, f.val())
				}
			default:
				writeBulk(w, f.val())
			}
		}
	case "GET":
		if f.inTx {
			f.queue = append(f.queue, cmd[1])
			writeSimple(w, "QUEUED")
			return
		}
		writeBulk(w, f.val())
	case "INCR": // answered with a bulk string: a reply type the typed slot did not expect
		writeBulk(w, f.val())
	default:
		writeSimple(w, "OK")
	}
}

// syncSlabClient returns a Client (pool size 1, so every Exec reuses one conn)
// whose conn is on the sync path, and the fake it talks to.
func syncSlabClient(t *testing.T) (*Client, *syncSlabFake) {
	t.Helper()
	f := &syncSlabFake{}
	f.fill.Store('a')
	fake := startFakeRedis(t, f.handle)
	cl, err := NewClient(fake.Addr(), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.pool.releaseCmd(c)
	if c.useDirect || c.syncMulti == nil {
		t.Fatalf("fixture: want a sync-path conn, got useDirect=%v syncMulti=%v", c.useDirect, c.syncMulti)
	}
	return cl, f
}

func txOfGets(t *testing.T, cl *Client, ctx context.Context, n int) []*StringCmd {
	t.Helper()
	tx, err := cl.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmds := make([]*StringCmd, n)
	for i := range cmds {
		cmds[i] = tx.Get("k")
	}
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("Tx.Exec: %v", err)
	}
	return cmds
}

func pipeOfGets(t *testing.T, cl *Client, ctx context.Context, n int) {
	t.Helper()
	p := cl.Pipeline()
	for i := 0; i < n; i++ {
		p.Get("k")
	}
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("Pipeline.Exec: %v", err)
	}
	p.Release()
}

// TestSyncTxResultsSurviveLaterExec1265 keeps the results of a Tx (10 x 4 KiB
// values) across a later Exec with different bytes on the same conn.
func TestSyncTxResultsSurviveLaterExec1265(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, second := range []string{"pipeline", "tx"} {
		t.Run("then_"+second, func(t *testing.T) {
			cl, f := syncSlabClient(t)
			const n = 10
			cmds := txOfGets(t, cl, ctx, n)
			want := strings.Repeat("a", 4096)
			f.fill.Store('b')
			if second == "tx" {
				txOfGets(t, cl, ctx, n)
			} else {
				pipeOfGets(t, cl, ctx, n)
			}
			for i, c := range cmds {
				got, err := c.Result()
				if err != nil {
					t.Fatalf("cmd %d: %v", i, err)
				}
				if got != want {
					t.Fatalf("Tx result %d was overwritten by a later Exec on the same conn: %d bytes, first byte %q, %d 'b' bytes (want 4096 x 'a')",
						i, len(got), got[:1], strings.Count(got, "b"))
				}
			}
		})
	}
}

// TestSyncPipelineFallbackResultSurvivesLaterExec1265 is the Pipeline form: a
// typed slot whose reply has another type (INCR answered with a bulk string)
// keeps a decoded Value in the Pipeline (pc.val), which on the sync path is a
// view into the slab.
func TestSyncPipelineFallbackResultSurvivesLaterExec1265(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, f := syncSlabClient(t)
	first := cl.Pipeline()
	incr := first.Incr("c")
	if err := first.Exec(ctx); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if first.cmds[0].val == nil {
		t.Fatal("fixture: the mismatched INCR reply was not kept as a Value")
	}
	f.fill.Store('b')
	pipeOfGets(t, cl, ctx, 10)
	_ = incr
	got := string(first.cmds[0].val.Str)
	if want := strings.Repeat("a", 4096); got != want {
		t.Fatalf("a Pipeline result kept from an earlier Exec was overwritten by a later Exec on the same conn: %d bytes, %d 'b' bytes (want 4096 x 'a')",
			len(got), strings.Count(got, "b"))
	}
}

// TestSyncTxResultsRaceWithLaterExec1265 is the concurrent form the issue
// describes: one goroutine reads Tx results while another runs Execs on the
// same conn. Under -race the unsynchronised read of slab bytes that the Exec
// rewrites is reported.
func TestSyncTxResultsRaceWithLaterExec1265(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, f := syncSlabClient(t)
	cmds := txOfGets(t, cl, ctx, 10)
	f.fill.Store('b')
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			pipeOfGets(t, cl, ctx, 10)
		}
	}()
	for i := 0; i < 20; i++ {
		for _, c := range cmds {
			_, _ = c.Result()
		}
	}
	wg.Wait()
}

// TestSyncTxNestedResultsSurviveLaterExec1265 keeps a Tx whose EXEC array holds
// nested aggregates (an array and a map of 4 KiB strings) across a later Exec
// on the same conn, then checks every nested byte.
func TestSyncTxNestedResultsSurviveLaterExec1265(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, f := syncSlabClient(t)
	tx, err := cl.TxPipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	arr := tx.Get("arr")
	mp := tx.Get("map")
	if err := tx.Exec(ctx); err != nil {
		t.Fatalf("Tx.Exec: %v", err)
	}
	av, mv := arr.pc.val, mp.pc.val
	if av == nil || len(av.Array) != 3 || mv == nil || len(mv.Map) != 2 {
		t.Fatalf("fixture: nested replies not decoded as 3-array and 2-map: %+v %+v", av, mv)
	}
	f.fill.Store('b')
	pipeOfGets(t, cl, ctx, 10)
	want := strings.Repeat("a", 4096)
	check := func(what string, b []byte) {
		if string(b) != want {
			t.Errorf("nested %s was overwritten by a later Exec on the same conn: %d bytes, %d 'b' bytes (want 4096 x 'a')", what, len(b), strings.Count(string(b), "b"))
		}
	}
	for i, e := range av.Array {
		check("array element "+string(rune('0'+i)), e.Str)
	}
	for i, kv := range mv.Map {
		check("map key "+string(rune('0'+i)), kv.K.Str)
		check("map value "+string(rune('0'+i)), kv.V.Str)
	}
}
