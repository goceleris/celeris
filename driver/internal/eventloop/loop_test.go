package eventloop

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

// tcpPair returns two connected TCP sockets and their dup'd file descriptors.
// The caller must close both conns and both fds. Works on every platform Go
// supports; the fd is a dup so closing it is independent from the net.Conn.
func tcpPair(t *testing.T) (left, right net.Conn, leftFD, rightFD int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type pair struct {
		c   net.Conn
		err error
	}
	ch := make(chan pair, 1)
	go func() {
		c, err := ln.Accept()
		ch <- pair{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	server := got.c

	clientFD := dupTCPFD(t, client)
	serverFD := dupTCPFD(t, server)
	return server, client, serverFD, clientFD
}

func dupTCPFD(t *testing.T, c net.Conn) int {
	t.Helper()
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("not a TCPConn: %T", c)
	}
	f, err := tcp.File()
	if err != nil {
		t.Fatalf("File(): %v", err)
	}
	fd := int(f.Fd())
	// net.FileConn on non-linux dups again; return the dup so closing the
	// *os.File does not drop the reference we just handed out.
	dup, err := dupFD(fd)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	_ = f.Close()
	return dup
}

func TestRegisterUnregister(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	left, right, leftFD, rightFD := tcpPair(t)
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	var (
		recv    []byte
		recvMu  sync.Mutex
		recvCh  = make(chan struct{}, 4)
		closed  atomic.Bool
		closeCh = make(chan error, 1)
	)
	onRecv := func(b []byte) {
		recvMu.Lock()
		recv = append(recv, b...)
		recvMu.Unlock()
		select {
		case recvCh <- struct{}{}:
		default:
		}
	}
	onClose := func(err error) {
		if closed.CompareAndSwap(false, true) {
			closeCh <- err
		}
	}

	w := l.WorkerLoop(0)
	if err := w.RegisterConn(leftFD, onRecv, onClose); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}

	// Peer (right) writes bytes; our registered side (left) should see them.
	if _, err := right.Write([]byte("hello")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	select {
	case <-recvCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onRecv")
	}
	recvMu.Lock()
	got := string(recv)
	recvMu.Unlock()
	if got != "hello" {
		t.Fatalf("onRecv got %q want %q", got, "hello")
	}

	// Our side writes back; peer must see the bytes.
	if err := w.Write(leftFD, []byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 32)
	_ = right.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := right.Read(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Fatalf("peer got %q want %q", buf[:n], "world")
	}

	if err := w.UnregisterConn(leftFD); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	select {
	case err := <-closeCh:
		if err != nil {
			t.Fatalf("onClose: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onClose")
	}
	_ = closeFD(leftFD)
	_ = closeFD(rightFD)

	if err := w.UnregisterConn(leftFD); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("double unregister: got %v want ErrUnknownFD", err)
	}
}

func TestResolveStandalone(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	p1, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	p2, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if p1 != p2 {
		t.Fatalf("standalone providers differ across Resolve: %p vs %p", p1, p2)
	}
	Release(p1)
	// Still one refcount outstanding, so the loop must survive.
	p3, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #3: %v", err)
	}
	if p3 != p1 {
		t.Fatalf("standalone replaced before refcount drained")
	}
	Release(p2)
	Release(p3)

	// With refcount zero, the next Resolve should spawn a fresh loop.
	p4, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve #4: %v", err)
	}
	if p4 == p1 {
		t.Fatalf("expected new standalone after full release")
	}
	Release(p4)
}

type mockProvider struct{}

func (m *mockProvider) NumWorkers() int                    { return 1 }
func (m *mockProvider) WorkerLoop(n int) engine.WorkerLoop { return nil }

type fakeServer struct {
	p engine.EventLoopProvider
}

func (f *fakeServer) EventLoopProvider() engine.EventLoopProvider { return f.p }

func TestResolveServerPassthrough(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	mock := &mockProvider{}
	got, err := Resolve(&fakeServer{p: mock})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != engine.EventLoopProvider(mock) {
		t.Fatalf("Resolve returned %v, want mock provider", got)
	}
	// Release on a server-backed provider must be a no-op and must not
	// touch the standalone loop.
	Release(got)

	standaloneMu.Lock()
	if standalone != nil {
		standaloneMu.Unlock()
		t.Fatal("server-backed Resolve must not spawn a standalone loop")
	}
	standaloneMu.Unlock()
}

func TestResolveNilServer(t *testing.T) {
	resetStandaloneForTest()
	t.Cleanup(resetStandaloneForTest)

	// Server that reports no EventLoopProvider (e.g. net/http fallback).
	got, err := Resolve(&fakeServer{p: nil})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil {
		t.Fatal("expected standalone provider, got nil")
	}
	Release(got)
}

func TestConcurrentRegistrations(t *testing.T) {
	const pairs = 32
	l, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	type entry struct {
		left, right     net.Conn
		leftFD, rightFD int
		recv            chan []byte
		closed          chan error
	}
	entries := make([]*entry, pairs)
	for i := range entries {
		a, b, aFD, bFD := tcpPair(t)
		entries[i] = &entry{
			left: a, right: b, leftFD: aFD, rightFD: bFD,
			recv:   make(chan []byte, 1),
			closed: make(chan error, 1),
		}
	}
	t.Cleanup(func() {
		for _, e := range entries {
			_ = e.left.Close()
			_ = e.right.Close()
			_ = closeFD(e.leftFD)
			_ = closeFD(e.rightFD)
		}
	})

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			idx := l.workerForFD(e.leftFD)
			w := l.WorkerLoop(idx)
			err := w.RegisterConn(e.leftFD, func(b []byte) {
				cp := append([]byte(nil), b...)
				select {
				case e.recv <- cp:
				default:
				}
			}, func(err error) {
				select {
				case e.closed <- err:
				default:
				}
			})
			if err != nil {
				t.Errorf("RegisterConn[%d]: %v", e.leftFD, err)
			}
		}(e)
	}
	wg.Wait()

	// Write a distinctive byte from each peer and assert the callback
	// fires on the matching registration.
	for i, e := range entries {
		b := []byte{byte(i + 1)}
		if _, err := e.right.Write(b); err != nil {
			t.Fatalf("peer %d write: %v", i, err)
		}
	}
	for i, e := range entries {
		select {
		case got := <-e.recv:
			if len(got) == 0 || got[0] != byte(i+1) {
				t.Fatalf("peer %d: got %v want [%d]", i, got, i+1)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("peer %d: onRecv timeout", i)
		}
	}

	for _, e := range entries {
		idx := l.workerForFD(e.leftFD)
		w := l.WorkerLoop(idx)
		if err := w.UnregisterConn(e.leftFD); err != nil {
			t.Fatalf("UnregisterConn: %v", err)
		}
	}
}
