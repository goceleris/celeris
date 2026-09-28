//go:build linux

package iouring

import "testing"

// BenchmarkWriteHooks measures the per-response cost of the write hooks the
// H1 response adapter calls (makeWriteFn, makeWriteBodyFn), whose
// back-pressure check celeris#761 changed: a header block and a small body
// through writeFn, and a header block and a 16 KiB zero-copy body through
// writeFn + writeBodyFn. The buffers are reset as a completed send leaves
// them.
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
	b.Run("zero-copy-body", func(b *testing.B) {
		w := &Worker{}
		cs := &connState{}
		wf, wb := w.makeWriteFn(cs), w.makeWriteBodyFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			wf(hdr)
			wb(large)
			cs.writeBuf = cs.writeBuf[:0]
			cs.bodyBuf = nil
		}
	})
}
