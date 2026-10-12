package eventloop

// celeris#887 item 19 sets a write deadline (one time.Now and one
// SetWriteDeadline) on every portable Write. This benchmark runs the same code
// on main and on the change, so benchstat compares the two directly. The
// portable worker is the development and test path of darwin and windows; the
// Linux workers do not use it.

import (
	"io"
	"net"
	"testing"
)

func BenchmarkPortableWrite887(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	b.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, c) // the server reads everything
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	l, err := NewPortable(1)
	if err != nil {
		b.Fatalf("NewPortable: %v", err)
	}
	b.Cleanup(func() { _ = l.Close() })
	id, err := l.WorkerLoop(0).(NetConnRegistrar).RegisterNetConn(client, func([]byte) {}, func(error) {})
	if err != nil {
		b.Fatalf("RegisterNetConn: %v", err)
	}
	w := l.WorkerLoop(0)
	req := make([]byte, 64)
	b.ReportAllocs()
	for b.Loop() {
		if err := w.Write(id, req); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}
