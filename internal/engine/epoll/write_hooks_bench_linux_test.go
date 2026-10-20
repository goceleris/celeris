//go:build linux

package epoll

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// BenchmarkWriteHooks measures the per-response cost of the write hooks the
// H1 response adapter calls (makeWriteFn, makeWriteBodyFn), whose
// back-pressure check celeris#761 changed, and of the flush that sends the
// response: a header block and a small body through writeFn, and a header
// block and a 16 KiB zero-copy body through writeFn + writeBodyFn, which
// since celeris#817 makes the writev(2) itself. The conn writes to /dev/null,
// so the syscall is in every arm and the flush is complete.
//
// The two short-write arms (celeris#892 item 16) write the zero-copy body to a
// pipe of one page, which cannot take it, so the branches that copy the body
// into writeBuf are measured, with -benchmem: the partial writev, where
// flushWritesV copies what the kernel did not take, and the full pipe, where
// the writev takes nothing and unstage copies the whole body. Each checks its
// premise on the first call and fails if the pipe took more or less than the
// arm is named for.
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
	b.Run("zero-copy-body-short-write", func(b *testing.B) {
		g := newShortWriteRig892(b)
		// The pipe is empty: the writev takes one page of [header, body] and
		// flushWritesV copies the rest of the body into writeBuf.
		g.premise(b, len(g.hdr), len(g.hdr)+len(g.body)-1, "partial writev")
		b.ReportAllocs()
		for b.Loop() {
			g.respond()
			g.reset()
			g.drain(b)
		}
	})
	b.Run("zero-copy-body-pipe-full", func(b *testing.B) {
		g := newShortWriteRig892(b)
		// The pipe is full and stays so: the writev takes nothing (EAGAIN) and
		// unstage copies the whole body into writeBuf.
		g.fill(b)
		g.premise(b, 0, 0, "full pipe")
		b.ReportAllocs()
		for b.Loop() {
			g.respond()
			g.reset()
		}
	})
}

// shortWriteRig892 is a conn that writes to a non-blocking pipe of the
// smallest capacity the kernel gives (one page), and a body larger than four of
// them, so that no writev of header and body is taken whole.
type shortWriteRig892 struct {
	l        *Loop
	cs       *connState
	w        func([]byte)
	wb       func([]byte)
	rd, wr   int
	capacity int
	hdr      []byte
	body     []byte
	scratch  []byte
}

func newShortWriteRig892(b *testing.B) *shortWriteRig892 {
	b.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		b.Fatalf("pipe2: %v", err)
	}
	b.Cleanup(func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) })
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_SETPIPE_SZ, unix.Getpagesize()); err != nil {
		b.Fatalf("F_SETPIPE_SZ: %v", err)
	}
	capacity, err := unix.FcntlInt(uintptr(p[1]), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		b.Fatalf("F_GETPIPE_SZ: %v", err)
	}
	g := &shortWriteRig892{
		l:        &Loop{},
		cs:       &connState{fd: p[1]},
		rd:       p[0],
		wr:       p[1],
		capacity: capacity,
		hdr:      make([]byte, 121),
		// 16 KiB, the zero-copy body's size in the other arms, and at least
		// four pipes, so that a host whose smallest pipe is a 64 KiB page
		// still cannot take it.
		body:    make([]byte, max(16<<10, 4*capacity)),
		scratch: make([]byte, capacity),
	}
	g.w, g.wb = g.l.makeWriteFn(g.cs), g.l.makeWriteBodyFn(g.cs)
	return g
}

// respond is the H1 adapter's response: the header block through the write
// hook, then the body through the zero-copy hook, which makes the writev.
func (g *shortWriteRig892) respond() {
	g.w(g.hdr)
	g.wb(g.body)
}

// reset puts the buffers back as a completed send leaves them.
func (g *shortWriteRig892) reset() {
	g.cs.writeBuf = g.cs.writeBuf[:0]
	g.cs.writePos = 0
	g.cs.bodyBuf = nil
	g.cs.pendingBytes = 0
}

// drain empties the pipe.
func (g *shortWriteRig892) drain(b *testing.B) {
	for {
		if _, err := unix.Read(g.rd, g.scratch); err != nil {
			if err == unix.EAGAIN {
				return
			}
			b.Fatalf("drain the pipe: %v", err)
		}
	}
}

// fill makes the pipe full: a write of its whole capacity, then one more byte
// that must be refused.
func (g *shortWriteRig892) fill(b *testing.B) {
	b.Helper()
	if n, err := unix.Write(g.wr, make([]byte, g.capacity)); err != nil || n != g.capacity {
		b.Fatalf("fill the pipe: wrote %d of %d bytes: %v", n, g.capacity, err)
	}
	if n, err := unix.Write(g.wr, []byte{0}); err != unix.EAGAIN {
		b.Fatalf("PREMISE: the pipe took another byte after %d bytes (%d, %v): it is not full", g.capacity, n, err)
	}
}

// premise runs one response and requires that the pipe took between lo and hi
// bytes of its header and body, that the rest is in writeBuf (a copy: bodyBuf
// is nil, as the body belongs to the handler once its write returns), and that
// pendingBytes counts it. It leaves the rig reset and the pipe empty if it took
// anything.
func (g *shortWriteRig892) premise(b *testing.B, lo, hi int, what string) {
	b.Helper()
	before := g.l.bytesWrittenBatch
	g.respond()
	total := len(g.hdr) + len(g.body)
	took := int(g.l.bytesWrittenBatch - before)
	if took < lo || took > hi {
		b.Fatalf("PREMISE (%s): the pipe took %d bytes of the %d, want %d to %d (pipe capacity %d, page size %d)",
			what, took, total, lo, hi, g.capacity, unix.Getpagesize())
	}
	if g.cs.bodyBuf != nil || len(g.cs.writeBuf)-g.cs.writePos != total-took || g.cs.pendingBytes != total-took {
		b.Fatalf("PREMISE (%s): %d bytes should be queued as a copy in writeBuf, got writeBuf %d (pos %d), bodyBuf %d, pendingBytes %d",
			what, total-took, len(g.cs.writeBuf), g.cs.writePos, len(g.cs.bodyBuf), g.cs.pendingBytes)
	}
	g.reset()
	if took > 0 {
		g.drain(b)
	}
}
