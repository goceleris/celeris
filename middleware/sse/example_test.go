package sse_test

import (
	"fmt"
	"strconv"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// Stream a counter to the client every second; client disconnects close
// the context, which ends the loop cleanly.
func ExampleNew() {
	s := celeris.New(celeris.Config{})

	s.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: 30 * time.Second,
		Handler: func(c *sse.Client) {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			i := 0
			for {
				select {
				case <-c.Context().Done():
					return
				case <-ticker.C:
					if err := c.Send(sse.Event{
						Event: "tick",
						ID:    strconv.Itoa(i),
						Data:  "tick " + strconv.Itoa(i),
					}); err != nil {
						return
					}
					i++
				}
			}
		},
	}))

	fmt.Println("SSE handler installed at /events")
	// Output: SSE handler installed at /events
}

// Fan-out from a single publisher to N subscribers. Broker formats each
// event once into a PreparedEvent and dispatches the cached bytes to
// every subscriber's per-subscriber queue, so the FormatEvent cost
// stays constant as the subscriber count grows.
func ExampleBroker() {
	broker := sse.NewBroker(sse.BrokerConfig{
		SubscriberBuffer: 64,
		OnSlowSubscriber: func(_ *sse.Client, _ *sse.PreparedEvent) sse.BrokerPolicy {
			return sse.BrokerPolicyDrop
		},
	})

	s := celeris.New(celeris.Config{})
	s.GET("/events", sse.New(sse.Config{
		Handler: func(c *sse.Client) {
			unsub := broker.Subscribe(c)
			defer unsub()
			<-c.Context().Done()
		},
	}))

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for i := 0; ; i++ {
			<-ticker.C
			broker.Publish(sse.Event{
				ID:    strconv.Itoa(i),
				Event: "tick",
				Data:  "tick " + strconv.Itoa(i),
			})
		}
	}()

	fmt.Println("broker installed at /events")
	// Output: broker installed at /events
}

// Persist events through a ReplayStore so a reconnecting client can
// resume from its Last-Event-ID without losing in-flight events. The
// in-memory ring buffer keeps the last 256 events; swap to
// NewKVReplayStore for durability across restarts.
func ExampleNewRingBuffer() {
	store := sse.NewRingBuffer(256)

	s := celeris.New(celeris.Config{})
	s.GET("/events", sse.New(sse.Config{
		ReplayStore: store,
		Handler: func(c *sse.Client) {
			_ = c.Send(sse.Event{Data: "hello"})
		},
	}))

	fmt.Println("replay-enabled SSE handler installed at /events")
	// Output: replay-enabled SSE handler installed at /events
}

// Resume from a Last-Event-ID supplied by the browser's reconnect logic.
func ExampleClient_LastEventID() {
	handler := sse.New(sse.Config{
		Handler: func(c *sse.Client) {
			start := 0
			if id := c.LastEventID(); id != "" {
				if n, err := strconv.Atoi(id); err == nil {
					start = n + 1
				}
			}
			_ = c.Send(sse.Event{
				ID:   strconv.Itoa(start),
				Data: "resumed at " + strconv.Itoa(start),
			})
		},
	})

	_ = handler
	fmt.Println("ok")
	// Output: ok
}
