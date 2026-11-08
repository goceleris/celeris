package redis

// celeris#1266: the third branch of execManyCore (syncMulti == nil and not
// direct: the bridge / async branch) does not reset redisState.copySlab the
// way the sync branch and, since #1264, the direct branch do. Dispatch there
// reaches extractDirect (req.expect != expectNone is set by Pipeline.Exec),
// which appends every typed string reply to the slab, so the slab would grow
// by one batch per Exec. This test measures it.

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

func TestBridgePipelineCopySlabStaysBounded1266(t *testing.T) {
	const (
		nCmds  = 10
		valLen = 4096
		nExecs = 100
		maxCap = 64 << 10 // one batch is 40 KiB
	)
	val := strings.Repeat("a", valLen)
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "GET":
			writeBulk(w, val)
		default:
			writeSimple(w, "OK")
		}
	})
	cl, err := NewClient(fake.Addr(), WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// bridgeConn pins the fixture: the pooled conn is on the bridge branch.
	// On Linux the standalone loop gives the conn a syncMulti; clear it so
	// the conn takes the third branch (idle conn, nothing else uses it).
	bridgeSlabCap := func() int {
		c, err := cl.pool.acquireCmd(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.pool.releaseCmd(c)
		c.syncMulti = nil
		if c.useDirect || c.syncMulti != nil {
			t.Fatal("fixture: the pooled conn is not on the bridge branch")
		}
		return cap(c.state.copySlab)
	}
	bridgeSlabCap() // dial and pin the conn first

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
			if got, err := c.Result(); err != nil || got != val {
				t.Fatalf("Exec #%d cmd %d: len=%d err=%v", i, j, len(got), err)
			}
		}
		p.Release()
		sc := bridgeSlabCap()
		if sc > high {
			high = sc
		}
		if sc > maxCap {
			t.Fatalf("after Exec #%d the bridge-branch conn's copySlab has cap %d bytes (> %d): nothing resets it between Execs", i, sc, maxCap)
		}
	}
	t.Logf("copySlab high-water over %d Execs: %d bytes", nExecs, high)
}
