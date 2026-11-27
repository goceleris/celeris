//go:build linux

package eventloop

// celeris#1357 raised the per-write cap check (len(writeBuf)-writePos+len(data)
// > maxPendingBytes) from 4 MiB to 64 MiB and added a shrink-after-drain test
// on the full-drain path. This is the cost of the path they sit on: one Write
// of a small payload (a 64-byte command) on a conn whose peer reads, so the
// write drains at once. The same file runs on the base, so benchstat compares
// the two directly.

import (
	"testing"

	"golang.org/x/sys/unix"
)

func BenchmarkWriteSmall1357(b *testing.B) {
	l, err := New(1)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = l.Close() })
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		b.Fatalf("socketpair: %v", err)
	}
	a, p := fds[0], fds[1]
	if err := unix.SetNonblock(a, true); err != nil {
		b.Fatalf("nonblock: %v", err)
	}
	b.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(p) })
	_ = unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
	w := l.WorkerLoop(0)
	if err := w.RegisterConn(a, func([]byte) {}, func(error) {}); err != nil {
		b.Fatalf("RegisterConn: %v", err)
	}
	b.Cleanup(func() { _ = w.UnregisterConn(a) })
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1<<20)
		for {
			// p is blocking: the read sleeps until the Writes put bytes in.
			if n, err := unix.Read(p, buf); n <= 0 && err != unix.EINTR {
				close(done)
				return
			}
		}
	}()
	payload := make([]byte, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.Write(a, payload); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
	b.StopTimer()
	_ = unix.Shutdown(p, unix.SHUT_RDWR) // wakes the blocked read
	<-done
}
