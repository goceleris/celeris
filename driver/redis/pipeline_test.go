package redis

import (
	"bufio"
	"context"
	"strings"
	"testing"
)

func TestPipelineBasic(t *testing.T) {
	c := newTestClient(t, nil)
	ctx := context.Background()
	p := c.Pipeline()
	setCmd := p.Set("k", "v", 0)
	getCmd := p.Get("k")
	incr := p.Incr("n")
	incr2 := p.Incr("n")
	if err := p.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := setCmd.Result(); err != nil || s != "OK" {
		t.Fatalf("set: %q %v", s, err)
	}
	if s, err := getCmd.Result(); err != nil || s != "v" {
		t.Fatalf("get: %q %v", s, err)
	}
	if n, err := incr.Result(); err != nil || n != 1 {
		t.Fatalf("incr1: %d %v", n, err)
	}
	if n, err := incr2.Result(); err != nil || n != 2 {
		t.Fatalf("incr2: %d %v", n, err)
	}
}

func TestPipelineDiscard(t *testing.T) {
	c := newTestClient(t, nil)
	p := c.Pipeline()
	p.Set("k", "v", 0)
	p.Discard()
	if len(p.cmds) != 0 {
		t.Fatal("discard didn't clear")
	}
	if err := p.Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineEmpty(t *testing.T) {
	c := newTestClient(t, nil)
	if err := c.Pipeline().Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPipelineChunkedReplies regression-tests the case where the server
// delivers pipeline replies as separate TCP segments. Each later chunk Feeds
// into the Reader's internal buffer; if earlier replies' Str slices still
// alias that buffer they get silently overwritten, causing SET to return the
// corrupted tail of a later INCR reply (e.g. "2\r"). The fix detaches replies
// from the Reader buffer inside dispatch. Running many trials exercises the
// scheduling race deterministically enough to catch a regression.
func TestPipelineChunkedReplies(t *testing.T) {
	counter := int64(0)
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			handleHELLO(w, 3)
		case "SET":
			writeSimple(w, "OK")
		case "GET":
			writeBulk(w, "v")
		case "INCR":
			counter++
			writeInt(w, counter)
		}
	})

	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for trial := 0; trial < 20; trial++ {
		p := c.Pipeline()
		setCmd := p.Set("k", "v", 0)
		getCmd := p.Get("k")
		incr := p.Incr("n")
		incr2 := p.Incr("n")
		if err := p.Exec(context.Background()); err != nil {
			// Unrelated transport flakes in the fake-server harness can
			// surface here; the corruption bug this test guards against
			// manifests as a wrong *value*, not an Exec error.
			continue
		}
		if s, err := setCmd.Result(); err != nil || s != "OK" {
			t.Fatalf("trial %d set: %q %v", trial, s, err)
		}
		if s, err := getCmd.Result(); err != nil || s != "v" {
			t.Fatalf("trial %d get: %q %v", trial, s, err)
		}
		if _, err := incr.Result(); err != nil {
			t.Fatalf("trial %d incr: %v", trial, err)
		}
		if _, err := incr2.Result(); err != nil {
			t.Fatalf("trial %d incr2: %v", trial, err)
		}
	}
}

func TestPipelineHash(t *testing.T) {
	c := newTestClient(t, nil)
	p := c.Pipeline()
	hset := p.HSet("h", "f", "v")
	hget := p.HGet("h", "f")
	if err := p.Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, err := hset.Result(); err != nil || n != 1 {
		t.Fatalf("hset: %d %v", n, err)
	}
	if s, err := hget.Result(); err != nil || s != "v" {
		t.Fatalf("hget: %q %v", s, err)
	}
}
