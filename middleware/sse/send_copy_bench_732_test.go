package sse

import (
	"strings"
	"testing"

	"github.com/goceleris/celeris/celeristest"
)

// The cost of the copies Send makes of what it keeps (celeris#732): one
// stream of 100 events with a 6-byte type and 64 bytes of data, as in
// BenchmarkBurstEvents (blocking, no replay store: nothing is kept, the
// control). Ring: blocking with a NewRingBuffer store, which copies each
// appended event. Queued: MaxQueueDepth 200 (nothing dropped) and no store,
// so Send copies each event onto the queue. QueuedRing: both; the ring keeps
// the queued copy, so one copy per event.
const burst732 = 100

func benchSendBurst732(b *testing.B, cfg Config) {
	e := Event{Event: "update", Data: strings.Repeat("x", 64)}
	cfg.HeartbeatInterval = -1
	cfg.Handler = func(client *Client) {
		for range burst732 {
			if err := client.Send(e); err != nil {
				return
			}
		}
	}
	handler := New(cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx := newDiscardContext()
		_ = handler(ctx)
		celeristest.ReleaseContext(ctx)
	}
}

func BenchmarkSendBurstRing732(b *testing.B) {
	benchSendBurst732(b, Config{ReplayStore: NewRingBuffer(1024)})
}

func BenchmarkSendBurstQueued732(b *testing.B) {
	benchSendBurst732(b, Config{MaxQueueDepth: 2 * burst732})
}

func BenchmarkSendBurstQueuedRing732(b *testing.B) {
	benchSendBurst732(b, Config{MaxQueueDepth: 2 * burst732, ReplayStore: NewRingBuffer(1024)})
}
