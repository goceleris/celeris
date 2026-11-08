package redis

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
)

// celeris#1268: a Pipeline.Exec that fails after it acquired a conn must give
// the pool slot back. Every error execManyCore returns closes the conn first,
// and releaseCmd discards a closed conn, which drops Open and frees the slot.
// Without that call a PoolSize-1 client has no slot left after one failed Exec:
// the next acquire blocks until its context ends.
//
// The tests go through the public client, on both command paths: the direct
// (net.Conn) one, which NewClient picks for a server that runs AsyncHandlers,
// and the default event-loop one.

const (
	leakHealthy1268  int32 = iota
	leakStall1268          // GET is read and never answered
	leakFIN1268            // GET gets half a reply, then the server closes the socket
	leakErrReply1268       // GET gets an error reply
	leakGarbage1268        // GET gets bytes that are not RESP
)

// leakServer1268 is a fake Redis whose answer to GET is switched by mode.
type leakServer1268 struct {
	fake *fakeRedis
	mode atomic.Int32
}

func startLeakServer1268(t *testing.T) *leakServer1268 {
	t.Helper()
	s := &leakServer1268{}
	var fp atomic.Pointer[fakeRedis]
	s.fake = startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			switch s.mode.Load() {
			case leakStall1268:
			case leakFIN1268:
				_, _ = w.WriteString("$5\r\nab")
				_ = w.Flush()
				// Close from another goroutine: the serve loop holds this
				// handler's writer, and the close is the FIN under test.
				f := fp.Load()
				go func() {
					f.mu.Lock()
					conns := append([]net.Conn(nil), f.conns...)
					f.mu.Unlock()
					for _, c := range conns {
						_ = c.Close()
					}
				}()
			case leakErrReply1268:
				writeError(w, "ERR boom")
			case leakGarbage1268:
				_, _ = w.WriteString("?not resp\r\n")
			default:
				writeBulk(w, "v")
			}
		default:
			writeSimple(w, "OK")
		}
	})
	fp.Store(s.fake)
	return s
}

// newLeakClient1268 opens a PoolSize-1 client on the public API. direct picks
// the AsyncHandlers path (a server that runs AsyncHandlers, not started: it is
// only the ServerProvider the option takes); otherwise the default event-loop
// path. It checks which path the pooled conn is on.
func newLeakClient1268(t *testing.T, direct bool) (*Client, *leakServer1268) {
	t.Helper()
	srv := startLeakServer1268(t)
	opts := []Option{WithPoolSize(1)}
	if direct {
		opts = append(opts, WithEngine(celeris.New(celeris.Config{
			Addr:          "127.0.0.1:0",
			Engine:        celeris.Std,
			AsyncHandlers: true,
		})))
	}
	cl, err := NewClient(srv.fake.Addr(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.useDirect != direct {
		t.Fatalf("fixture: the pooled conn has useDirect = %v, want %v", c.useDirect, direct)
	}
	cl.pool.releaseCmd(c)
	return cl, srv
}

func wantStats1268(t *testing.T, cl *Client, when string, open, idle, inUse int) {
	t.Helper()
	st := cl.Stats()
	if st.Open != open || st.Idle != idle || st.InUse != inUse {
		t.Errorf("%s: pool Open/Idle/InUse = %d/%d/%d, want %d/%d/%d", when, st.Open, st.Idle, st.InUse, open, idle, inUse)
	}
}

// wantOneSlot1268 checks that the pool of one hands out exactly one conn: a
// second acquire must block while the first is held. A slot released twice
// frees capacity that is not there, and this sees it.
func wantOneSlot1268(t *testing.T, cl *Client, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, err := cl.pool.acquireCmd(ctx, 0)
	if err != nil {
		t.Errorf("%s: acquire: %v", when, err)
		return
	}
	defer cl.pool.releaseCmd(c1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	c2, err := cl.pool.acquireCmd(ctx2, 0)
	if err == nil {
		cl.pool.releaseCmd(c2)
		t.Errorf("%s: a pool of one handed out a second conn while the first was held", when)
	}
}

// getOK1268 runs one healthy single-GET Exec and fails the test unless it
// succeeds. A leaked slot shows here as the acquire timing out.
func getOK1268(t *testing.T, cl *Client, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := cl.Pipeline()
	defer p.Release()
	g := p.Get("a")
	if err := p.Exec(ctx); err != nil {
		t.Errorf("%s: Exec: %v", when, err)
		return
	}
	if v, err := g.Result(); err != nil || v != "v" {
		t.Errorf("%s: GET = %q, %v; want \"v\", nil", when, v, err)
	}
}

func pathName1268(direct bool) string {
	if direct {
		return "direct"
	}
	return "default"
}

// bounded1268 runs f, which reports with t.Errorf only, and fails the test if it
// does not return. A slot released twice makes the next release block on the
// pool's semaphore for good: that is a failure of these tests, not a hang.
func bounded1268(t *testing.T, f func()) {
	t.Helper()
	within1250(t, "the pool to answer (a slot released twice blocks it)", f)
}

// A failed Exec (the deadline passes with no reply, or the server closes the
// socket mid-reply) returns its slot: the conn is closed, so it is dropped, and
// a second Exec dials a new one.
func TestPipelineFailedExecReleasesItsSlot1268(t *testing.T) {
	cases := []struct {
		name    string
		mode    int32
		timeout time.Duration
		gets    int // the server closes after its first partial reply, so one command
	}{
		{"timeout", leakStall1268, 50 * time.Millisecond, 2},
		{"server closes mid-reply", leakFIN1268, 5 * time.Second, 1},
	}
	for _, direct := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(pathName1268(direct)+"/"+tc.name, func(t *testing.T) {
				cl, srv := newLeakClient1268(t, direct)
				bounded1268(t, func() {
					srv.mode.Store(tc.mode)
					ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
					p := cl.Pipeline()
					for i := 0; i < tc.gets; i++ {
						p.Get("a")
					}
					err := p.Exec(ctx)
					cancel()
					if err == nil {
						t.Error("Exec succeeded against a server that did not answer")
					}
					p.Release()
					wantStats1268(t, cl, "after the failed Exec", 0, 0, 0)

					srv.mode.Store(leakHealthy1268)
					getOK1268(t, cl, "Exec after the failed one")
					wantStats1268(t, cl, "after the healthy Exec", 1, 1, 0)
					wantOneSlot1268(t, cl, "after fail then success")
				})
			})
		}
	}
}

// Replies that make Exec return an error. An error reply leaves a healthy conn
// that the success path hands back; it passes on main. A reply that is not RESP
// closes the conn. On the direct path and on the default path off Linux the
// parser fails the requests and Exec's success path gives the closed conn to
// releaseCmd, which drops it: that passes on main. On the default path on Linux
// the sync poll does not end the Exec on it: it returns at its context deadline
// (2 s here) through execManyCore's error return, the branch of #1268, so the
// case fails on main there.
func TestPipelineErrorRepliesReleaseTheirSlot1268(t *testing.T) {
	cases := []struct {
		name              string
		mode              int32
		open, idle, inUse int // pool stats after the failed Exec
	}{
		{"error reply", leakErrReply1268, 1, 1, 0},
		{"not RESP", leakGarbage1268, 0, 0, 0},
	}
	for _, direct := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(pathName1268(direct)+"/"+tc.name, func(t *testing.T) {
				cl, srv := newLeakClient1268(t, direct)
				bounded1268(t, func() {
					srv.mode.Store(tc.mode)
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					p := cl.Pipeline()
					g := p.Get("a")
					if err := p.Exec(ctx); err == nil {
						t.Error("Exec returned no error")
					}
					if _, err := g.Result(); err == nil {
						t.Error("the command has no error")
					}
					p.Release()
					wantStats1268(t, cl, "after the failed Exec", tc.open, tc.idle, tc.inUse)

					srv.mode.Store(leakHealthy1268)
					getOK1268(t, cl, "Exec after the failed one")
					wantStats1268(t, cl, "after the healthy Exec", 1, 1, 0)
					wantOneSlot1268(t, cl, "after fail then success")
				})
			})
		}
	}
}

// Negative control: Execs that succeed release their conn once. Open stays 1,
// the slot count stays exact (a double release would free capacity the pool
// does not have, and a pool of one would then hand out two conns).
func TestPipelineSuccessReleasesItsSlotOnce1268(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(pathName1268(direct), func(t *testing.T) {
			cl, _ := newLeakClient1268(t, direct)
			bounded1268(t, func() {
				for i := 0; i < 5; i++ {
					getOK1268(t, cl, "Exec")
					wantStats1268(t, cl, "after a successful Exec", 1, 1, 0)
				}
				wantOneSlot1268(t, cl, "after successful Execs")
			})
		})
	}
}

// Failures in a row do not shrink the pool: each returns its slot, so a client
// of one conn survives many of them.
func TestPipelineRepeatedFailedExecsKeepThePool1268(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(pathName1268(direct), func(t *testing.T) {
			cl, srv := newLeakClient1268(t, direct)
			bounded1268(t, func() {
				for i := 0; i < 3; i++ {
					srv.mode.Store(leakStall1268)
					ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
					p := cl.Pipeline()
					p.Get("a")
					err := p.Exec(ctx)
					cancel()
					p.Release()
					if err == nil {
						t.Errorf("failure %d: Exec succeeded", i)
					}
					srv.mode.Store(leakHealthy1268)
					getOK1268(t, cl, "Exec after a failure")
				}
				wantStats1268(t, cl, "after three failures and three successes", 1, 1, 0)
				wantOneSlot1268(t, cl, "after repeated failures")
			})
		})
	}
}

// ClusterPipeline runs its sub-pipelines through Pipeline.Exec on each node's
// client, so a timed-out sub-pipeline took the node's slot with it.
func TestClusterPipelineFailedExecReleasesItsSlot1268(t *testing.T) {
	var mode atomic.Int32
	fcn := &fakeClusterNode{mem: newMem(), startSlot: 0, endSlot: 16383}
	fcn.fake = startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "GET") && mode.Load() == leakStall1268 {
			return
		}
		fcn.handler(cmd, w)
	})
	fcn.SetTopology([]fakeClusterSlotRange{{start: 0, end: 16383, addr: fcn.Addr()}})
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{fcn.Addr()},
		PoolSize:    1,
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	nodeStats := func() PoolStats {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.nodes[fcn.Addr()].client.Stats()
	}
	bounded1268(t, func() {
		setup, cancelSetup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelSetup()
		if err := cc.Set(setup, "k", "v", 0); err != nil {
			t.Error(err)
			return
		}

		mode.Store(leakStall1268)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		cp := cc.Pipeline()
		cp.Get("k")
		_, errs := cp.Exec(ctx)
		cancel()
		if errs[0] == nil {
			t.Error("ClusterPipeline.Exec succeeded against a node that did not answer")
		}
		if st := nodeStats(); st.InUse != 0 || st.Open != 0 {
			t.Errorf("after the failed Exec: node pool Open/Idle/InUse = %d/%d/%d, want 0/0/0", st.Open, st.Idle, st.InUse)
		}

		mode.Store(leakHealthy1268)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		cp2 := cc.Pipeline()
		cp2.Get("k")
		vals, errs2 := cp2.Exec(ctx2)
		if errs2[0] != nil || string(vals[0].Str) != "v" {
			t.Errorf("ClusterPipeline.Exec after the failed one = %q, %v; want \"v\", nil", vals[0].Str, errs2[0])
		}
		if st := nodeStats(); st.Open != 1 || st.Idle != 1 || st.InUse != 0 {
			t.Errorf("after the healthy Exec: node pool Open/Idle/InUse = %d/%d/%d, want 1/1/0", st.Open, st.Idle, st.InUse)
		}
	})
}

// Tx releases its conn in a defer, so a failed Exec already returned its slot.
// A guard for the neighbouring path: it passes on the unfixed code.
func TestTxFailedExecReleasesItsSlot1268(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(pathName1268(direct), func(t *testing.T) {
			cl, srv := newLeakClient1268(t, direct)
			bounded1268(t, func() {
				srv.mode.Store(leakStall1268)
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				tx, err := cl.TxPipeline(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				tx.Get("a")
				if err := tx.Exec(ctx); err == nil {
					t.Error("Tx.Exec succeeded against a server that did not answer")
				}
				wantStats1268(t, cl, "after the failed Tx.Exec", 0, 0, 0)

				srv.mode.Store(leakHealthy1268)
				getOK1268(t, cl, "Exec after the failed Tx.Exec")
				wantStats1268(t, cl, "after the healthy Exec", 1, 1, 0)
			})
		})
	}
}
