package redis

// RULE 74 micro-benchmark for celeris#887 item 22: one GET per op over a fake
// sync round tripper that answers at once, so the figure is the driver's own
// sync path (pollSync's deferred unlock) and nothing else. Uses only symbols
// that exist on main, so benchstat compares main and the fix directly.

import (
	"context"
	"testing"
)

type replySync struct{ reply []byte }

func (r replySync) WriteAndPoll(_ int, _ []byte, _ []byte, onRecv func([]byte)) (bool, error) {
	onRecv(r.reply)
	return true, nil
}

func BenchmarkExec2Sync887(b *testing.B) {
	c := &redisConn{state: newRedisState(), sync: replySync{reply: []byte("$1\r\nv\r\n")}}
	c.onRecvFn = c.onRecv
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		req, err := c.exec2(ctx, "GET", "k")
		if err != nil {
			b.Fatal(err)
		}
		c.releaseResult(req)
	}
}
