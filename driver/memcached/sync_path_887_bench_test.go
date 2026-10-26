package memcached

// RULE 74 micro-benchmark for celeris#887 item 22: one text command per op
// over a fake sync round tripper that answers at once, so the figure is the
// driver's own sync path (pollSync's deferred unlock) and nothing else. Uses
// only symbols that exist on main, so benchstat compares main and the fix
// directly.

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/internal/driver/memcached/protocol"
)

type replySync struct{ reply []byte }

func (r replySync) WriteAndPoll(_ int, _ []byte, _ []byte, onRecv func([]byte)) (bool, error) {
	onRecv(r.reply)
	return true, nil
}

func BenchmarkExecTextSync887(b *testing.B) {
	c := &memcachedConn{state: newMCState(false), sync: replySync{reply: []byte("VERSION 1.6.0\r\n")}}
	c.onRecvFn = c.onRecv
	ctx := context.Background()
	enc := func(w *protocol.TextWriter) []byte { return w.AppendSimple("version") }
	b.ReportAllocs()
	for b.Loop() {
		req, err := c.execText(ctx, kindVersion, enc)
		if err != nil {
			b.Fatal(err)
		}
		putRequest(req)
	}
}
