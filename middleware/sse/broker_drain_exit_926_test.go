package sse

import (
	"errors"
	"testing"
	"time"
)

// TestBrokerDrainExitUnsubscribesFirst926 (review of celeris#926): when a
// subscriber's drain goroutine exits on a failed write, its subscription must
// be gone by the time the drain is: the handler's teardown waits only for the
// drain (brokerRefs) before the Client goes back to its pool, and a Client
// pooled while still in the broker's map gets the next owner's Subscribe
// ignored and a publisher's slow path acting on it. The removal used to come
// only from the context.AfterFunc goroutine that the failed write's cancel
// starts, which can run after the drain has finished. Each round checks the
// map at the moment the drain's done channel closes.
func TestBrokerDrainExitUnsubscribesFirst926(t *testing.T) {
	const rounds = 200
	late := 0
	for i := range rounds {
		b := NewBroker(BrokerConfig{})
		ctx, ms := newSSEContext(t)
		ready := make(chan *Client, 1)
		stop := make(chan struct{})
		handlerDone := make(chan struct{})
		go func() {
			defer close(handlerDone)
			_ = New(Config{HeartbeatInterval: -1, Handler: func(c *Client) {
				ready <- c
				<-stop
			}})(ctx)
		}()
		c := <-ready
		b.Subscribe(c) // the handler never unsubscribes
		b.mu.RLock()
		s := b.subscribers[c]
		b.mu.RUnlock()
		ms.mu.Lock()
		ms.writeErr = errors.New("client gone")
		ms.mu.Unlock()
		b.Publish(Event{Data: "x"})
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the drain did not exit after its write failed", i)
		}
		b.mu.RLock()
		_, still := b.subscribers[c]
		b.mu.RUnlock()
		if still {
			late++
		}
		close(stop)
		<-handlerDone
		b.Close()
	}
	t.Logf("%d of %d rounds: still subscribed when the drain had exited", late, rounds)
	if late != 0 {
		t.Errorf("in %d of %d rounds the subscriber was still registered after its drain exited", late, rounds)
	}
}
