//go:build linux

package redis

import (
	"bufio"
	"context"
	"testing"
)

// BenchmarkPubSubControlWrite929 measures one Subscribe/Unsubscribe pair on a
// live pubsub conn: the path celeris#929 put a mutex on (ctlMu). It is the
// RULE 74 measurement for that lock; the command pool's exec path does not
// take it.
func BenchmarkPubSubControlWrite929(b *testing.B) {
	br := newBroker()
	fake := startFakeRedisBench(b, func(cmd []string, w *bufio.Writer) { br.handler(cmd, w) })
	br.fake = fake
	cl, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cl.Close() })
	ps, err := cl.newPubSub(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ps.Close() })
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ps.Subscribe(ctx, "ch"); err != nil {
			b.Fatal(err)
		}
		if err := ps.Unsubscribe(ctx, "ch"); err != nil {
			b.Fatal(err)
		}
	}
}
