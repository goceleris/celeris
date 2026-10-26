package redis

// celeris#887 items 22 and 25 (driver/redis), both found by code reading and
// never run; each test is a probe of the property the item says is broken.
//
//   - 22: a panic (or Goexit) that leaves WriteAndPoll / WriteAndPollMulti
//     (a RESP3 OnPush handler runs inside it on the sync path) must not leave
//     writerMu locked with the conn open: the next command on a conn the
//     caller still holds blocks in writerMu.Lock, which has no ctx. The probe
//     injects the panic through the sync / syncMulti interface fields, the
//     seam the real eventloop worker is installed through.
//   - 25: drainWithError's notify callback writes resultErr and finishes a
//     request after the drain took its ring. A caller whose wait returned on
//     ctx.Done (after closeWithErr) releases the request to reqPool, so a drain
//     still walking its snapshot writes into a request the pool may already
//     have handed to the next command.

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

type panicSync struct{ goexit bool }

func (p panicSync) WriteAndPoll(int, []byte, []byte, func([]byte)) (bool, error) {
	if p.goexit {
		runtime.Goexit()
	}
	panic("an OnPush handler panicked inside WriteAndPoll")
}

func (p panicSync) WriteAndPollMulti(int, []byte, []byte, func([]byte), func() bool, func()) (bool, error) {
	if p.goexit {
		runtime.Goexit()
	}
	panic("an OnPush handler panicked inside WriteAndPollMulti")
}

func newSyncFaultConn(p panicSync) *redisConn {
	c := &redisConn{state: newRedisState(), sync: p, syncMulti: p}
	c.onRecvFn = c.onRecv
	c.pollIsDoneFn = c.pollIsDone
	c.pollBeforeRearmFn = c.pollBeforeRearm
	return c
}

var syncFaultCalls = []struct {
	name string
	run  func(c *redisConn)
}{
	{"exec", func(c *redisConn) { _, _ = c.exec(context.Background(), "PING") }},
	{"exec2", func(c *redisConn) { _, _ = c.exec2(context.Background(), "GET", "k") }},
	{"exec3", func(c *redisConn) { _, _ = c.exec3(context.Background(), "SET", "k", "v") }},
	{"execMany", func(c *redisConn) { _, _ = c.execMany(context.Background(), []byte("*1\r\n$4\r\nPING\r\n"), 1) }},
}

func checkNotWedged(t *testing.T, c *redisConn, what string) {
	t.Helper()
	if !c.writerMu.TryLock() {
		t.Fatalf("writerMu is still locked after %s: the next command blocks in writerMu.Lock for good", what)
	}
	c.writerMu.Unlock()
	if !c.closed.Load() {
		t.Errorf("conn not marked broken after %s left its parser mid-frame", what)
	}
}

func TestSyncPollPanicLeavesConnUsableOrBroken887(t *testing.T) {
	for _, sc := range syncFaultCalls {
		t.Run(sc.name, func(t *testing.T) {
			c := newSyncFaultConn(panicSync{})
			func() {
				defer func() { _ = recover() }()
				sc.run(c)
			}()
			checkNotWedged(t, c, "a recovered panic")
		})
	}
}

func TestSyncPollGoexitLeavesConnUsableOrBroken887(t *testing.T) {
	for _, sc := range syncFaultCalls {
		t.Run(sc.name, func(t *testing.T) {
			c := newSyncFaultConn(panicSync{goexit: true})
			done := make(chan struct{})
			go func() {
				defer close(done)
				sc.run(c)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the goroutine did not end")
			}
			checkNotWedged(t, c, "a Goexit")
		})
	}
}

// A drain that took its snapshot (onClose on the loop; the parse-error drain in
// onRecv) is still walking it when a caller whose ctx is done returns from
// wait: closeWithErr's own drain finds the ring empty, so the caller does not
// wait for the walker. The caller then releases the request; the walker must
// not write to it afterwards (the pool may have handed it to the next
// command, on another conn: a spurious error, or the new command's own reply
// dropped because finished is already set).
//
// The window is forced without a hook: the ring holds a few hundred thousand
// requests, the victim last, so the walker is still busy when the caller runs;
// the caller starts once the snapshot has been taken (the ring is empty).
func TestDrainDoesNotWriteAReleasedRequest887(t *testing.T) {
	const fillers = 400_000
	for attempt := 0; attempt < 10; attempt++ {
		c := &redisConn{state: newRedisState()}
		bg := context.Background()
		for i := 0; i < fillers; i++ {
			c.state.bridge.Enqueue(getRequest(bg))
		}
		ctx, cancel := context.WithCancel(bg)
		cancel()
		victim := getRequest(ctx)
		c.state.bridge.Enqueue(victim)

		walked := make(chan struct{})
		go func() { c.onClose(io.EOF); close(walked) }()
		for c.state.bridge.Len() != 0 {
			runtime.Gosched()
		}

		err := c.wait(ctx, victim)
		if !errors.Is(err, context.Canceled) {
			<-walked
			continue // the walker reached the victim first: not the window under test
		}
		// What a command does next: release the request.
		c.releaseResult(victim)
		errAt, finAt := victim.resultErr, victim.finished.Load()
		<-walked
		if victim.resultErr != errAt || victim.finished.Load() != finAt {
			t.Fatalf("the drain wrote to a request after its caller released it: resultErr %v -> %v, finished %v -> %v",
				errAt, victim.resultErr, finAt, victim.finished.Load())
		}
		return
	}
	t.Skip("the walker finished before the caller in every attempt; the window was not reached")
}
