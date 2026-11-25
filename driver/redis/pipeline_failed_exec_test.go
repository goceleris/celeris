package redis

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests pin the behaviour of a Pipeline whose Exec FAILED, which v1.5.11
// defines and which must not change: the queue is cleared, new commands can be
// queued, and the next Exec sends only those. They use only API and test
// helpers that v1.5.11 has, so the same file runs there for the comparison.
// The "already executed" error is for an Exec after a SUCCESSFUL Exec only.

// stallHandler answers SET and every GET except GET "stall", which it never
// answers (the Exec of a pipeline holding it fails on the caller's deadline or
// when the server drops the conn).
func stallHandler(cmd []string, w *bufio.Writer) {
	switch strings.ToUpper(cmd[0]) {
	case "HELLO":
		handleHELLO(w, 3)
	case "SET":
		writeSimple(w, "OK")
	case "GET":
		if cmd[1] == "stall" {
			return
		}
		writeBulk(w, "val-"+cmd[1])
	}
}

func newStallClient(t *testing.T) (*Client, *fakeRedis) {
	t.Helper()
	fake := startFakeRedis(t, stallHandler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fake
}

// failedExec queues a stalling GET and runs Exec to a failure.
func failedExec(t *testing.T, c *Client, fake *fakeRedis, dropConn bool) (*Pipeline, *StringCmd, error) {
	t.Helper()
	p := c.Pipeline()
	g := p.Get("stall")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if dropConn {
		go func() {
			time.Sleep(100 * time.Millisecond)
			fake.mu.Lock()
			conns := fake.conns
			fake.mu.Unlock()
			for _, cn := range conns {
				_ = cn.Close()
			}
		}()
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
	}
	err := p.Exec(ctx)
	if err == nil {
		t.Fatal("Exec against a server that never answers succeeded")
	}
	return p, g, err
}

func TestPipelineRetryAfterFailedExec(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
	}{{"deadline", false}, {"dropped-conn", true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, fake := newStallClient(t)
			p, _, ferr := failedExec(t, c, fake, tc.drop)
			t.Logf("first Exec error: %v; pool %+v", ferr, c.Stats())
			defer p.Release()

			// No Discard: queue again and Exec again, as v1.5.11 allowed.
			s := p.Set("k", "v", 0)
			g := p.Get("other")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.Exec(ctx); err != nil {
				t.Fatalf("Exec after a failed Exec (retry without Discard): %v", err)
			}
			if v, err := s.Result(); err != nil || v != "OK" {
				t.Fatalf("SET = %q, %v", v, err)
			}
			if v, err := g.Result(); err != nil || v != "val-other" {
				t.Fatalf("GET = %q, %v", v, err)
			}
			// And the pipeline, having succeeded, runs once more only after
			// a Discard.
			p.Discard()
			g2 := p.Get("again")
			if err := p.Exec(ctx); err != nil {
				t.Fatalf("Exec after Discard: %v", err)
			}
			if v, err := g2.Result(); err != nil || v != "val-again" {
				t.Fatalf("GET after Discard = %q, %v", v, err)
			}
		})
	}
}

// An Exec that failed before taking a connection leaves the queue intact: the
// same Exec can simply be run again (v1.5.11 behaviour).
func TestPipelineRetryAfterExecFailedBeforeSending(t *testing.T) {
	c, _ := newStallClient(t)
	p := c.Pipeline()
	defer p.Release()
	g := p.Get("k")
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Exec(dead); err == nil {
		t.Fatal("Exec with a canceled context succeeded")
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := p.Exec(ctx); err != nil {
		t.Fatalf("retry of the same Exec: %v", err)
	}
	if v, err := g.Result(); err != nil || v != "val-k" {
		t.Fatalf("GET = %q, %v", v, err)
	}
}

// After a failed Exec with nothing queued again, v1.5.11's Exec had an empty
// queue: it sent nothing and returned nil. It must not block.
func TestPipelineExecAfterFailedExecWithNothingQueued(t *testing.T) {
	c, fake := newStallClient(t)
	p, _, _ := failedExec(t, c, fake, false)
	defer p.Release()
	done := make(chan error, 1)
	go func() { done <- p.Exec(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Exec with an empty queue = %v, want nil as in v1.5.11", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Exec after a failed Exec blocked")
	}
}

// A Result read after a failed Exec reports the Exec's own error.
func TestPipelineResultAfterFailedExecReportsTheRealError(t *testing.T) {
	c, fake := newStallClient(t)
	p, g, execErr := failedExec(t, c, fake, false)
	defer p.Release()
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("Exec = %v, want the deadline", execErr)
	}
	_, e := g.Result()
	if e == nil || errors.Is(e, ErrClosed) {
		t.Fatalf("Result after a failed Exec = %v, want the Exec's error", e)
	}
}

// A second Exec after a SUCCESSFUL Exec returns an error at once and does not
// touch a connection.
func TestPipelineSecondExecAfterSuccessErrorsAtOnce(t *testing.T) {
	c, _ := newStallClient(t)
	p := c.Pipeline()
	defer p.Release()
	g := p.Get("k")
	if err := p.Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Exec(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("second Exec returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second Exec blocked")
	}
	if st := c.Stats(); st.InUse != 0 {
		t.Fatalf("pool after the second Exec: %+v", st)
	}
	if v, err := g.Result(); err != nil || v != "val-k" {
		t.Fatalf("first Exec's result = %q, %v", v, err)
	}
}
