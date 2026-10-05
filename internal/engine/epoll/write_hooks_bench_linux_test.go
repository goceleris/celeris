//go:build linux

package epoll

import (
	"os"
	"testing"
)

// BenchmarkWriteHooks measures the per-response cost of the write hooks the
// H1 response adapter calls (makeWriteFn, makeWriteBodyFn), whose
// back-pressure check celeris#761 changed, and of the flush that sends the
// response: a header block and a small body through writeFn, and a header
// block and a 16 KiB zero-copy body through writeFn + writeBodyFn, which
// since celeris#817 makes the writev(2) itself. The conn writes to /dev/null,
// so the syscall is in every arm and the flush is complete.
func BenchmarkWriteHooks(b *testing.B) {
	hdr := make([]byte, 121)
	small := make([]byte, 64)
	large := make([]byte, 16<<10)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = devNull.Close() }()
	fd := int(devNull.Fd())
	b.Run("small", func(b *testing.B) {
		l := &Loop{}
		cs := &connState{fd: fd}
		w := l.makeWriteFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			w(hdr)
			w(small)
			if err := l.flushWrites(cs, true); err != nil {
				b.Fatal(err)
			}
			cs.pendingBytes = 0
		}
	})
	b.Run("zero-copy-body", func(b *testing.B) {
		l := &Loop{}
		cs := &connState{fd: fd}
		w, wb := l.makeWriteFn(cs), l.makeWriteBodyFn(cs)
		b.ReportAllocs()
		for b.Loop() {
			w(hdr)
			wb(large)
			if err := l.flushWrites(cs, true); err != nil {
				b.Fatal(err)
			}
			cs.pendingBytes = 0
		}
	})
}
