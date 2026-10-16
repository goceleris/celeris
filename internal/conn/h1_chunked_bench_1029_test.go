package conn

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

type nopHandler1029 struct{}

func (nopHandler1029) HandleStream(context.Context, *stream.Stream) error { return nil }

// BenchmarkProcessH1Chunked1029 is the read hot path around the chunked branch
// of ProcessH1 (celeris#1029, RULE 74). "get" and "post-cl" go through
// the branches the change touches without entering it (the resume check at the
// top, the header-deadline arm condition); "chunked-1call" is the path a
// chunked body that arrives whole takes; "chunked-2calls" and
// "chunked-many-calls" are the split paths that main answers 400 (they only
// run on the fix).
func BenchmarkProcessH1Chunked1029(b *testing.B) {
	run := func(b *testing.B, segs [][]byte) {
		st := NewH1State()
		st.MaxRequestBodySize = 4 << 20
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
				if err := ProcessH1(ctx, s, st, nopHandler1029{}, w); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	b.Run("get", func(b *testing.B) {
		run(b, [][]byte{[]byte("GET /x HTTP/1.1\r\nHost: a\r\n\r\n")})
	})
	for _, n := range []int{64, 4096, 65536} {
		body := bytes.Repeat([]byte{'x'}, n)
		cl := append([]byte(fmt.Sprintf("POST /up HTTP/1.1\r\nHost: a\r\nContent-Length: %d\r\n\r\n", n)), body...)
		b.Run(fmt.Sprintf("post-cl/%d", n), func(b *testing.B) { run(b, [][]byte{cl}) })
		wire := append([]byte(benchHead1029), benchChunked1029(body, 4096)...)
		b.Run(fmt.Sprintf("chunked-1call/%d", n), func(b *testing.B) { run(b, [][]byte{wire}) })
		if testing.Short() {
			continue
		}
		b.Run(fmt.Sprintf("chunked-2calls/%d", n), func(b *testing.B) {
			if !splitWorks1029() {
				b.Skip("a chunked body over two calls is answered 400 here (celeris#1029)")
			}
			run(b, benchCut1029(wire, (len(wire)+1)/2))
		})
		b.Run(fmt.Sprintf("chunked-many-calls/%d", n), func(b *testing.B) {
			if !splitWorks1029() {
				b.Skip("a chunked body over several calls is answered 400 here (celeris#1029)")
			}
			run(b, benchCut1029(wire, 1460))
		})
	}
}

// BenchmarkProcessH1Pipelined1029: k tiny POSTs pipelined into one ProcessH1
// call (review round 1: a burst of pipelined chunked requests must cost O(k),
// not the O(k*n) of moving every pipelined byte once per chunked request). The
// rows are at about the default BufferSize (k=120), 64 KiB (k=1024) and under
// MaxBufferSize (k=3600, 244800 B); "cl" is the Content-Length control.
func BenchmarkProcessH1Pipelined1029(b *testing.B) {
	for _, k := range []int{120, 1024, 3600} {
		for _, kind := range []string{"chunked", "cl"} {
			var wire bytes.Buffer
			for i := 0; i < k; i++ {
				if kind == "chunked" {
					wire.WriteString("POST /u HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n")
				} else {
					wire.WriteString("POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\n\r\nx")
				}
			}
			data := wire.Bytes()
			b.Run(fmt.Sprintf("%s/k=%d", kind, k), func(b *testing.B) {
				st := NewH1State()
				st.MaxRequestBodySize = 4 << 20
				h := &countHandler1029{}
				w := func([]byte) {}
				ctx := context.Background()
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := ProcessH1(ctx, data, st, h, w); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if h.n != k*b.N {
					b.Fatalf("served %d requests, want %d", h.n, k*b.N)
				}
			})
		}
	}
}

// splitWorks1029 reports whether a chunked body over two calls is served.
func splitWorks1029() bool {
	st := NewH1State()
	var out bytes.Buffer
	w := func(b []byte) { out.Write(b) }
	h := &countHandler1029{}
	wire := append([]byte(benchHead1029), benchChunked1029([]byte("hello"), 5)...)
	for _, seg := range benchCut1029(wire, len(wire)-3) {
		if err := ProcessH1(context.Background(), seg, st, h, w); err != nil {
			return false
		}
	}
	return h.n == 1
}

type countHandler1029 struct{ n int }

func (h *countHandler1029) HandleStream(context.Context, *stream.Stream) error { h.n++; return nil }

const benchHead1029 = "POST /up HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n"

// benchChunked1029 encodes body in chunks of size chunk and the terminator.
func benchChunked1029(body []byte, chunk int) []byte {
	var b bytes.Buffer
	for off := 0; off < len(body); off += chunk {
		end := min(off+chunk, len(body))
		fmt.Fprintf(&b, "%x\r\n", end-off)
		b.Write(body[off:end])
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n\r\n")
	return b.Bytes()
}

func benchCut1029(b []byte, k int) [][]byte {
	var segs [][]byte
	for len(b) > 0 {
		n := min(k, len(b))
		segs = append(segs, b[:n])
		b = b[n:]
	}
	return segs
}
