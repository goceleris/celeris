package postgres

import (
	"context"
	"testing"
	"time"
)

// BenchmarkPoolQueryCtxKinds is BenchmarkPoolQuery (a direct Pool against a fake
// loopback backend, so a statement is a few microseconds and any per-statement
// cost of the context shows at its largest) by kind of context: Background and a
// value context register nothing; a cancel-only context and one with a deadline
// register a context.AfterFunc for the statement. The file uses only API that
// exists on main, so the identical file can be dropped into a tree without the
// watch to compare.
func BenchmarkPoolQueryCtxKinds(b *testing.B) {
	type kv struct{}
	for _, tc := range []struct {
		name string
		mk   func() (context.Context, context.CancelFunc)
	}{
		{"background", func() (context.Context, context.CancelFunc) { return context.Background(), func() {} }},
		{"value", func() (context.Context, context.CancelFunc) {
			return context.WithValue(context.Background(), kv{}, 1), func() {}
		}},
		{"cancel", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"timeout", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := rowsBenchPool(b)
			ctx, cancel := tc.mk()
			defer cancel()
			var v int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := p.QueryContext(ctx, "SELECT 7")
				if err != nil {
					b.Fatal(err)
				}
				if rows.Next() {
					if err := rows.Scan(&v); err != nil {
						b.Fatal(err)
					}
				}
				_ = rows.Close()
			}
		})
	}
}
