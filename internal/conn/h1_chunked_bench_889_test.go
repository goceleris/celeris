package conn

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// BenchmarkProcessH1ChunkedL9889 is the read hot path around the changes of
// celeris#889 (lane L9, RULE 74): the engines run ProcessH1 inline
// (InlineMode, a RouteAsync that says no) on every conn that is not promoted.
//
//   - chunked-1call, post-cl, get: the paths the framing counter and the
//     one-chunk view touch without a pipelined request behind them.
//   - chunked+get/k: a chunked POST with k small GETs pipelined behind it,
//     parsed in the buffered loop, which now asks RouteAsync for each of them.
//     RouteAsync here is a string compare; the router's is a lookup, so the
//     row is the lower bound of the cost.
//   - split-2calls, split-many: the resume path.
//   - one-chunk: one large chunk in 64 KiB pieces (a body the handler gets as
//     a view of state.buffer instead of a copy).
func BenchmarkProcessH1ChunkedL9889(b *testing.B) {
	run := func(b *testing.B, segs [][]byte, want int) {
		st := NewH1State()
		st.MaxRequestBodySize = 4 << 20
		st.InlineMode = true
		st.RouteAsync = func(_, path string) bool { return path == "/async" }
		h := &countHandler1029{}
		w := func([]byte) {}
		ctx := context.Background()
		n := 0
		for _, s := range segs {
			n += len(s)
		}
		b.SetBytes(int64(n))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, s := range segs {
				if err := ProcessH1(ctx, s, st, h, w); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		if h.n != want*b.N {
			b.Fatalf("served %d requests, want %d", h.n, want*b.N)
		}
	}
	get := []byte("GET /x HTTP/1.1\r\nHost: a\r\n\r\n")
	b.Run("get", func(b *testing.B) { run(b, [][]byte{get}, 1) })
	for _, n := range []int{64, 4096} {
		body := bytes.Repeat([]byte{'x'}, n)
		cl := append([]byte(fmt.Sprintf("POST /up HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n\r\n", n)), body...)
		b.Run(fmt.Sprintf("post-cl/%d", n), func(b *testing.B) { run(b, [][]byte{cl}, 1) })
		wire := append([]byte(benchHead1029), benchChunked1029(body, 1024)...)
		b.Run(fmt.Sprintf("chunked-1call/%d", n), func(b *testing.B) { run(b, [][]byte{wire}, 1) })
		for _, k := range []int{1, 8} {
			with := append([]byte(nil), wire...)
			for i := 0; i < k; i++ {
				with = append(with, get...)
			}
			b.Run(fmt.Sprintf("chunked+get/%d/k=%d", n, k), func(b *testing.B) { run(b, [][]byte{with}, 1+k) })
		}
		b.Run(fmt.Sprintf("split-2calls/%d", n), func(b *testing.B) {
			run(b, benchCut1029(wire, (len(wire)+1)/2), 1)
		})
	}
	big := bytes.Repeat([]byte{'x'}, 256<<10)
	bigWire := append([]byte(benchHead1029), benchChunked1029(big, len(big))...)
	b.Run("one-chunk/256K/piece64K", func(b *testing.B) { run(b, benchCut1029(bigWire, 64<<10), 1) })
	multi := append([]byte(benchHead1029), benchChunked1029(big, 32<<10)...)
	b.Run("multi-chunk/256K/piece64K", func(b *testing.B) { run(b, benchCut1029(multi, 64<<10), 1) })
}
