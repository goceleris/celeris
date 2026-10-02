//go:build linux

package iouring

import "testing"

// BenchmarkWriteHooks measures the per-response cost of the write hook the
// H1 response adapter calls (makeWriteFn), whose back-pressure check
// celeris#761 changed: a header block and a small body, and a header block
// and a 16 KiB body. The adapter copies every body through makeWriteFn since
// io_uring has no zero-copy body writer (celeris#817), so the 16 KiB case
// includes that copy. The buffers are reset as a completed send leaves them.
func BenchmarkWriteHooks(b *testing.B) {
	hdr := make([]byte, 121)
	small := make([]byte, 64)
	large := make([]byte, 16<<10)
	b.Run("small", func(b *testing.B) {
		w := &Worker{}
		cs := &connState{}
		wf := w.makeWriteFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			wf(hdr)
			wf(small)
			cs.writeBuf = cs.writeBuf[:0]
		}
	})
	b.Run("large-body", func(b *testing.B) {
		w := &Worker{}
		cs := &connState{}
		wf := w.makeWriteFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			wf(hdr)
			wf(large)
			cs.writeBuf = cs.writeBuf[:0]
		}
	})
}
