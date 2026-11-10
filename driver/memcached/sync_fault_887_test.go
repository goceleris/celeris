package memcached

// celeris#887 items 22 and 25 (driver/memcached), both found by code reading
// and never run; each test is a probe of the property the item says is broken.
// See driver/redis/sync_fault_887_test.go for the reasoning; the probes are the
// same, over memcached's sync call sites (text, binary, binary multi).

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/memcached/protocol"
)

type panicSync struct{ goexit bool }

func (p panicSync) WriteAndPoll(int, []byte, []byte, func([]byte)) (bool, error) {
	if p.goexit {
		runtime.Goexit()
	}
	panic("an onRecv callback panicked inside WriteAndPoll")
}

func (p panicSync) WriteAndPollMulti(int, []byte, []byte, func([]byte), func() bool, func()) (bool, error) {
	if p.goexit {
		runtime.Goexit()
	}
	panic("an onRecv callback panicked inside WriteAndPollMulti")
}

func newSyncFaultConn(p panicSync, bin, multi bool) *memcachedConn {
	c := &memcachedConn{state: newMCState(bin), sync: p, binary: bin}
	if multi {
		c.syncMulti = p
	}
	c.onRecvFn = c.onRecv
	c.pollIsDoneFn = c.pollIsDone
	return c
}

var syncFaultCalls = []struct {
	name       string
	bin, multi bool
	run        func(c *memcachedConn)
}{
	{"execText", false, false, func(c *memcachedConn) {
		_, _ = c.execText(context.Background(), kindVersion, func(w *protocol.TextWriter) []byte { return w.AppendSimple("version") })
	}},
	{"execBinary", true, false, func(c *memcachedConn) {
		_, _ = c.execBinary(context.Background(), protocol.OpVersion, func(w *protocol.BinaryWriter, o uint32) []byte {
			return w.AppendSimple(protocol.OpVersion, o)
		})
	}},
	{"execBinaryMulti/poll", true, false, func(c *memcachedConn) {
		_, _ = c.execBinaryMulti(context.Background(), func(w *protocol.BinaryWriter) []byte {
			return w.AppendSimple(protocol.OpNoop, 1)
		}, func(protocol.BinaryPacket) bool { return true })
	}},
	{"execBinaryMulti/multi", true, true, func(c *memcachedConn) {
		_, _ = c.execBinaryMulti(context.Background(), func(w *protocol.BinaryWriter) []byte {
			return w.AppendSimple(protocol.OpNoop, 1)
		}, func(protocol.BinaryPacket) bool { return true })
	}},
}

func checkNotWedged(t *testing.T, c *memcachedConn, what string) {
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
			c := newSyncFaultConn(panicSync{}, sc.bin, sc.multi)
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
			c := newSyncFaultConn(panicSync{goexit: true}, sc.bin, sc.multi)
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

// See the redis twin: a drain still walking its snapshot must not write to a
// request whose caller already released it.
func TestDrainDoesNotWriteAReleasedRequest887(t *testing.T) {
	const fillers = 400_000
	for attempt := 0; attempt < 10; attempt++ {
		c := &memcachedConn{state: newMCState(false)}
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
		putRequest(victim)
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
