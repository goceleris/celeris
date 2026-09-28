//go:build linux

package epoll

import "testing"

// BenchmarkWriteHooks measures the per-response cost of the write hooks the
// H1 response adapter calls (makeWriteFn, makeWriteBodyFn), whose
// back-pressure check celeris#761 changed: a header block and a small body
// through writeFn, and a header block and a 16 KiB zero-copy body through
// writeFn + writeBodyFn. The buffers are reset as a completed flush leaves
// them.
func BenchmarkWriteHooks(b *testing.B) {
	hdr := make([]byte, 121)
	small := make([]byte, 64)
	large := make([]byte, 16<<10)
	b.Run("small", func(b *testing.B) {
		l := &Loop{}
		cs := &connState{}
		w := l.makeWriteFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			w(hdr)
			w(small)
			cs.writeBuf = cs.writeBuf[:0]
			cs.pendingBytes = 0
		}
	})
	b.Run("zero-copy-body", func(b *testing.B) {
		l := &Loop{}
		cs := &connState{}
		w, wb := l.makeWriteFn(cs), l.makeWriteBodyFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			w(hdr)
			wb(large)
			cs.writeBuf = cs.writeBuf[:0]
			cs.bodyBuf = nil
			cs.pendingBytes = 0
		}
	})
}
