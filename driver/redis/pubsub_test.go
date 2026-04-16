package redis

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// pubsubBroker is a tiny in-process broker. It tracks per-conn subscriptions
// and fan-outs from a publish channel. Writer synchronization is delegated to
// the owning *fakeRedis via WriterMutex so publisher and serve goroutines
// serialize their writes.
type pubsubBroker struct {
	mu        sync.Mutex
	subs      map[*bufio.Writer]map[string]struct{}
	shardSubs map[*bufio.Writer]map[string]struct{}
	fake      *fakeRedis // set after fake is started
}

func newBroker() *pubsubBroker {
	return &pubsubBroker{
		subs:      map[*bufio.Writer]map[string]struct{}{},
		shardSubs: map[*bufio.Writer]map[string]struct{}{},
	}
}

// handler runs on the serve goroutine under the fake's per-writer mutex, so
// direct writes and Flush are safe without additional locking here.
func (b *pubsubBroker) handler(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	switch strings.ToUpper(cmd[0]) {
	case "HELLO":
		handleHELLO(w, 3)
	case "SUBSCRIBE":
		b.mu.Lock()
		s, ok := b.subs[w]
		if !ok {
			s = map[string]struct{}{}
			b.subs[w] = s
		}
		for i, ch := range cmd[1:] {
			s[ch] = struct{}{}
			writeArrayHeader(w, 3)
			writeBulk(w, "subscribe")
			writeBulk(w, ch)
			writeInt(w, int64(i+1))
		}
		b.mu.Unlock()
	case "UNSUBSCRIBE":
		b.mu.Lock()
		if s, ok := b.subs[w]; ok {
			for _, ch := range cmd[1:] {
				delete(s, ch)
			}
		}
		b.mu.Unlock()
	case "SSUBSCRIBE":
		b.mu.Lock()
		s, ok := b.shardSubs[w]
		if !ok {
			s = map[string]struct{}{}
			b.shardSubs[w] = s
		}
		for i, ch := range cmd[1:] {
			s[ch] = struct{}{}
			writeArrayHeader(w, 3)
			writeBulk(w, "ssubscribe")
			writeBulk(w, ch)
			writeInt(w, int64(i+1))
		}
		b.mu.Unlock()
	case "SUNSUBSCRIBE":
		b.mu.Lock()
		if s, ok := b.shardSubs[w]; ok {
			for _, ch := range cmd[1:] {
				delete(s, ch)
			}
		}
		b.mu.Unlock()
	case "SPUBLISH":
		if len(cmd) >= 3 {
			b.spublish(cmd[1], cmd[2])
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "PING":
		writeSimple(w, "PONG")
	}
}

// spublish delivers shard channel messages ("smessage") to all shard
// subscribers of ch.
func (b *pubsubBroker) spublish(ch, payload string) {
	b.mu.Lock()
	targets := make([]*bufio.Writer, 0, len(b.shardSubs))
	for w, s := range b.shardSubs {
		if _, ok := s[ch]; ok {
			targets = append(targets, w)
		}
	}
	b.mu.Unlock()
	for _, w := range targets {
		wm := b.fake.WriterMutex(w)
		wm.Lock()
		writeArrayHeader(w, 3)
		writeBulk(w, "smessage")
		writeBulk(w, ch)
		writeBulk(w, payload)
		_ = w.Flush()
		wm.Unlock()
	}
}

// publish delivers msgs to all subscribers of ch, serializing its writes with
// the serve goroutine via the fake's per-writer mutex.
func (b *pubsubBroker) publish(ch, payload string) {
	b.mu.Lock()
	targets := make([]*bufio.Writer, 0, len(b.subs))
	for w, s := range b.subs {
		if _, ok := s[ch]; ok {
			targets = append(targets, w)
		}
	}
	b.mu.Unlock()
	for _, w := range targets {
		wm := b.fake.WriterMutex(w)
		wm.Lock()
		writeArrayHeader(w, 3)
		writeBulk(w, "message")
		writeBulk(w, ch)
		writeBulk(w, payload)
		_ = w.Flush()
		wm.Unlock()
	}
}

func TestPubSubBasic(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
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

	// Give the server time to process the SUBSCRIBE.
	time.Sleep(50 * time.Millisecond)
	b.publish("chan1", "hello")

	select {
	case m := <-ps.Channel():
		if m.Channel != "chan1" || m.Payload != "hello" {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPubSubUnsubscribe(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.Subscribe(context.Background(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	time.Sleep(50 * time.Millisecond)
	if err := ps.Unsubscribe(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	_, hasA := ps.subs["a"]
	_, hasB := ps.subs["b"]
	ps.mu.Unlock()
	if hasA || !hasB {
		t.Fatalf("unsub state bad: hasA=%v hasB=%v", hasA, hasB)
	}
}

func TestPubSubDeliverAfterClose(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.Subscribe(context.Background(), "ch")
	if err != nil {
		t.Fatal(err)
	}
	// Close first, then deliver. Before the fix, deliver would send on a
	// closed channel and panic.
	_ = ps.Close()
	// Must not panic.
	ok := ps.deliver(&Message{Channel: "ch", Payload: "late"})
	if ok {
		t.Fatal("deliver after close should return false")
	}
}

func TestSSubscribeBasic(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.newPubSub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	if err := ps.SSubscribe(context.Background(), "shard1"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	b.spublish("shard1", "hello-shard")

	select {
	case m := <-ps.Channel():
		if m.Channel != "shard1" || m.Payload != "hello-shard" {
			t.Fatalf("unexpected message: %+v", m)
		}
		if !m.Shard {
			t.Fatal("expected Shard=true for smessage")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shard message")
	}
}

func TestSSubscribeReconnect(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.newPubSub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	if err := ps.SSubscribe(context.Background(), "rshard"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Verify the shard sub is tracked.
	ps.mu.Lock()
	_, has := ps.shardSubs["rshard"]
	ps.mu.Unlock()
	if !has {
		t.Fatal("shardSubs should contain 'rshard'")
	}

	// Simulate a connection drop by closing the underlying conn.
	ps.mu.Lock()
	conn := ps.conn
	ps.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}

	// Wait for reconnect to re-establish.
	time.Sleep(500 * time.Millisecond)

	// Publish again after reconnect.
	b.spublish("rshard", "after-reconnect")

	select {
	case m := <-ps.Channel():
		if m.Channel != "rshard" || m.Payload != "after-reconnect" || !m.Shard {
			t.Fatalf("unexpected message after reconnect: %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message after reconnect")
	}
}

func TestSMessageRouting(t *testing.T) {
	b := newBroker()
	fake := startFakeRedis(t, b.handler)
	b.fake = fake
	c, err := NewClient(fake.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ps, err := c.newPubSub(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })

	// Subscribe to both regular and shard channels.
	if err := ps.Subscribe(context.Background(), "regular"); err != nil {
		t.Fatal(err)
	}
	if err := ps.SSubscribe(context.Background(), "shard"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Publish to both.
	b.publish("regular", "reg-payload")
	b.spublish("shard", "shard-payload")

	got := map[string]*Message{}
	for range 2 {
		select {
		case m := <-ps.Channel():
			got[m.Channel] = m
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for message")
		}
	}

	if m, ok := got["regular"]; !ok {
		t.Fatal("missing regular message")
	} else if m.Shard {
		t.Fatal("regular message should have Shard=false")
	} else if m.Payload != "reg-payload" {
		t.Fatalf("regular payload = %q, want %q", m.Payload, "reg-payload")
	}

	if m, ok := got["shard"]; !ok {
		t.Fatal("missing shard message")
	} else if !m.Shard {
		t.Fatal("shard message should have Shard=true")
	} else if m.Payload != "shard-payload" {
		t.Fatalf("shard payload = %q, want %q", m.Payload, "shard-payload")
	}
}
