package sse

import (
	"testing"
	"time"
)

// TestBrokerPublishDoesNotWaitForAStuckSubscriber926 is celeris#926. A
// subscriber whose write never returns (its client stopped reading) and
// whose queue is full gets the OnSlowSubscriber policy. Under
// BrokerPolicyClose the slow path called Client.Close, which takes the
// Client's mutex, held by the stuck write; under BrokerPolicyRemove it waited
// for the drain goroutine to write out the queue to that client. Either way
// PublishPrepared waited for the client that does not read, and with one
// publisher the whole broker stalled. Now the publish returns while the
// write is still stuck; when it returns the subscriber is unregistered, and
// under Close its context is already cancelled. BrokerPolicyDrop is the
// control: it never waited.
func TestBrokerPublishDoesNotWaitForAStuckSubscriber926(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     BrokerPolicy
		wantSubs   int
		wantCancel bool
	}{
		{"close", BrokerPolicyClose, 0, true},
		{"remove", BrokerPolicyRemove, 0, false},
		{"control/drop", BrokerPolicyDrop, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBroker(BrokerConfig{
				SubscriberBuffer: 1,
				OnSlowSubscriber: func(*Client, *PreparedEvent) BrokerPolicy { return tc.policy },
			})
			slow, release, cleanup, gate := startSlowClient(t)
			defer cleanup()
			defer b.Close()
			unsub := b.Subscribe(slow)
			defer unsub()
			defer release()
			// The drain takes fill1 and blocks in its write; fill2 fills the
			// queue; the next publish finds it full.
			b.Publish(Event{Data: "fill1"})
			select {
			case <-gate.writeReachedOnce:
			case <-time.After(2 * time.Second):
				t.Fatal("the subscriber's drain never reached its write")
			}
			b.Publish(Event{Data: "fill2"})

			published := make(chan struct{})
			go func() {
				b.Publish(Event{Data: "trigger"})
				close(published)
			}()
			select {
			case <-published:
			case <-time.After(2 * time.Second):
				t.Fatalf("Publish did not return within 2 s: it waits for the stuck subscriber's write")
			}
			if got := b.SubscriberCount(); got != tc.wantSubs {
				t.Errorf("SubscriberCount after the publish = %d, want %d", got, tc.wantSubs)
			}
			if cancelled := slow.Context().Err() != nil; cancelled != tc.wantCancel {
				t.Errorf("subscriber's context cancelled = %v, want %v", cancelled, tc.wantCancel)
			}
			if tc.policy == BrokerPolicyClose {
				// The stuck write returns; then the client is closed.
				release()
				deadline := time.Now().Add(2 * time.Second)
				for {
					gate.mu.Lock()
					closed := gate.closed
					gate.mu.Unlock()
					if closed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the client was not closed once its write returned")
					}
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

// TestBrokerSubscriberLeftSubscribedDoesNotHoldItsHandler926: the Client
// stays out of its pool while the broker's drain goroutine can still use it
// (celeris#926), and the drain also ends when the Client's context does. A
// handler that returns without calling its unsubscribe (the contract says it
// must) therefore still finishes: its teardown cancels the context, the
// drain exits, and the Client is released.
func TestBrokerSubscriberLeftSubscribedDoesNotHoldItsHandler926(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	defer b.Close()
	ctx, gate := newGatedContext(t)
	gate.release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = New(Config{HeartbeatInterval: -1, Handler: func(c *Client) { b.Subscribe(c) }})(ctx)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler's request did not finish: its teardown waits for the broker's drain")
	}
}

// TestBrokerCloseDoesNotWaitForAStuckSubscriber926 is the same family:
// Broker.Close closed every subscriber's queue and then waited for each drain
// goroutine, so a subscriber whose write never returns held Close, and with it
// the shutdown that called it. Close now returns once the queues are closed;
// each drain finishes on its own, and the handler's unsubscribe joins it.
func TestBrokerCloseDoesNotWaitForAStuckSubscriber926(t *testing.T) {
	b := NewBroker(BrokerConfig{})
	slow, release, cleanup, gate := startSlowClient(t)
	defer cleanup()
	unsub := b.Subscribe(slow)
	defer unsub()
	defer release()
	b.Publish(Event{Data: "stuck"})
	select {
	case <-gate.writeReachedOnce:
	case <-time.After(2 * time.Second):
		t.Fatal("the subscriber's drain never reached its write")
	}
	closed := make(chan struct{})
	go func() {
		b.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Broker.Close did not return within 2 s: it waits for the stuck subscriber's write")
	}
	if n := b.SubscriberCount(); n != 0 {
		t.Errorf("SubscriberCount after Close = %d, want 0", n)
	}
}
