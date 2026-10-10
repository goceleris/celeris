package stream

import (
	"bytes"
	"context"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// Benchmarks for the two hot paths celeris#951 and celeris#944 touch. They
// use only what the stream package had before both changes, so the same file
// builds against main and against the fix: the A/B in the PR runs the two
// binaries interleaved (evidence/lanes-20261009/H2/scripts/ab.sh).
//
//   - BenchmarkAsyncRequest951: one request on an async route, from its HEADERS
//     frame to the end of the event loop's frame batch. The handler runs on the
//     worker pool; its stream used to be released by the pool goroutine and is
//     now released by the loop at the batch end. loop-ns/op is the time the
//     event loop itself spent (ProcessRawHeaders and FlushInlineCleanup), where
//     the release moved to.
//   - BenchmarkHeadersDecode944: one inline request, which the HPACK emit
//     callback decodes, with no cookie, one cookie field and three cookie
//     fields (the join).

type asyncAlways struct{ done chan struct{} }

func (a *asyncAlways) HandleStream(context.Context, *Stream) error {
	a.done <- struct{}{}
	return nil
}
func (a *asyncAlways) RouteAsync(_, _ string) bool { return true }
func (a *asyncAlways) HasAsyncRoutes() bool        { return true }

func benchBlock(b *testing.B, extra ...hpack.HeaderField) []byte {
	b.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, f := range append([]hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/hello"}, {Name: ":authority", Value: "example.com"},
		{Name: "user-agent", Value: "bench"}, {Name: "accept", Value: "*/*"},
	}, extra...) {
		if err := enc.WriteField(f); err != nil {
			b.Fatal(err)
		}
	}
	return buf.Bytes()
}

func BenchmarkAsyncRequest951(b *testing.B) {
	h := &asyncAlways{done: make(chan struct{}, 1)}
	proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
	block := benchBlock(b)
	id := uint32(1)
	var loop time.Duration
	b.ReportAllocs()
	for b.Loop() {
		t0 := time.Now()
		if err := proc.ProcessRawHeaders(id, true, block); err != nil {
			b.Fatal(err)
		}
		loop += time.Since(t0)
		id += 2
		<-h.done
		t0 = time.Now()
		proc.FlushInlineCleanup() // the end of the frame batch
		loop += time.Since(t0)
	}
	b.ReportMetric(float64(loop.Nanoseconds())/float64(b.N), "loop-ns/op")
}

func BenchmarkHeadersDecode944(b *testing.B) {
	for _, tc := range []struct {
		name  string
		extra []hpack.HeaderField
	}{
		{"no-cookie", nil},
		{"one-cookie-field", []hpack.HeaderField{{Name: "cookie", Value: "theme=dark; sid=alice"}}},
		{"three-cookie-fields", []hpack.HeaderField{{Name: "cookie", Value: "theme=dark"}, {Name: "cookie", Value: "sid=alice"}, {Name: "cookie", Value: "lang=en"}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			proc := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
			block := benchBlock(b, tc.extra...)
			id := uint32(1)
			b.ReportAllocs()
			for b.Loop() {
				if err := proc.ProcessRawHeaders(id, true, block); err != nil {
					b.Fatal(err)
				}
				id += 2
			}
		})
	}
}
