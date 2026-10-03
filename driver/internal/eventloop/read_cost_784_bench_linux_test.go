//go:build linux

package eventloop

// The uncontended cost of celeris#784's fix on the read paths: every read(2)
// of a driver conn is issued under the conn's read lock after a closed check
// (readOpen), and a WriteAndPoll* call's two EPOLL_CTL_MODs under the conn's
// mutex after the same check (setEvents). These benchmarks run the same code
// on the base and on the fix, so benchstat compares the two directly. The
// worker has no goroutine of its own (newWorker, no run): each op drives one
// read path on the benchmark goroutine with a 64-byte response already
// queued, the shape of a small redis/memcached reply.
//
//	handleReadable: 2 reads per op (64 bytes, then EAGAIN), plus the peer's write;
//	                each op dispatches the conn's event, as the worker does
//	WriteAndPoll*:  3 reads per op (64 bytes, EAGAIN, the final drain's EAGAIN),
//	                plus the request write, two epoll_ctl MODs, and the peer's
//	                write and read
//
// The contended side, a read while another goroutine flushes, is
// read_contended_784_bench_linux_test.go.

import (
	"testing"

	"golang.org/x/sys/unix"
)

func bench784Worker(b *testing.B) (*worker, int, int) {
	b.Helper()
	w, err := newWorker(0)
	if err != nil {
		b.Fatalf("newWorker: %v", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		b.Fatalf("socketpair: %v", err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			b.Fatalf("nonblock: %v", err)
		}
	}
	b.Cleanup(func() {
		_ = w.shutdown()
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	if err := w.RegisterConn(fds[0], func([]byte) {}, func(error) {}); err != nil {
		b.Fatalf("RegisterConn: %v", err)
	}
	return w, fds[0], fds[1]
}

func BenchmarkHandleReadable784(b *testing.B) {
	w, fd, peer := bench784Worker(b)
	c := c784Lookup(w, fd)
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd), Pad: int32(c.gen)}
	resp := make([]byte, 64)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := unix.Write(peer, resp); err != nil {
			b.Fatalf("peer write: %v", err)
		}
		w.dispatch(ev)
	}
}

func BenchmarkWriteAndPoll784(b *testing.B) {
	type call func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error)
	isDone := func() bool { return true }
	for _, bc := range []struct {
		name string
		call call
	}{
		{"WriteAndPoll", func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPoll(fd, req, rbuf, onRecv)
		}},
		{"WriteAndPollBusy", func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollBusy(fd, req, rbuf, onRecv)
		}},
		{"WriteAndPollMulti", func(w *worker, fd int, req, rbuf []byte, onRecv func([]byte)) (bool, error) {
			return w.WriteAndPollMulti(fd, req, rbuf, onRecv, isDone, nil)
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			w, fd, peer := bench784Worker(b)
			req := make([]byte, 32)
			resp := make([]byte, 64)
			rbuf := make([]byte, 16<<10)
			sink := make([]byte, 256)
			onRecv := func([]byte) {}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := unix.Write(peer, resp); err != nil {
					b.Fatalf("peer write: %v", err)
				}
				ok, err := bc.call(w, fd, req, rbuf, onRecv)
				if !ok || err != nil {
					b.Fatalf("%s: (%v, %v)", bc.name, ok, err)
				}
				if _, err := unix.Read(peer, sink); err != nil {
					b.Fatalf("peer read: %v", err)
				}
			}
		})
	}
}
