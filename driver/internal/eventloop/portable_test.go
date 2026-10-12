package eventloop

// Tests of the portable (goroutine-per-connection) worker that backs the
// non-Linux loop. They talk to an in-process fake server over loopback TCP and
// register the net.Conn itself (RegisterNetConn), so they need no file
// descriptor and run on every platform: windows-latest and macos-latest run
// them as the fallback's own tests, and Linux CI runs the same logic here
// because portableWorker is compiled everywhere.

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

const portableWait = 5 * time.Second

// newPortableTestLoop returns a Loop of n portable workers: the Loop.Close
// path (cancel, wake, shutdown of every worker) is the production one.
func newPortableTestLoop(t *testing.T, n int) *Loop {
	t.Helper()
	l, err := NewPortable(n)
	if err != nil {
		t.Fatalf("NewPortable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func registrar(t *testing.T, l *Loop, n int) NetConnRegistrar {
	t.Helper()
	r, ok := l.WorkerLoop(n).(NetConnRegistrar)
	if !ok {
		t.Fatalf("worker %d (%T) does not implement NetConnRegistrar", n, l.WorkerLoop(n))
	}
	return r
}

// loopbackServer is an in-process server on loopback TCP: accept hands the test
// the server side of every connection the client dials.
type loopbackServer struct {
	ln    net.Listener
	conns chan net.Conn
}

func newLoopbackServer(t *testing.T) *loopbackServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &loopbackServer{ln: ln, conns: make(chan net.Conn, 64)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for {
			select {
			case c := <-s.conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return s
}

// dial returns the client side (the one the loop will own) and the server side.
func (s *loopbackServer) dial(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", s.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case srv := <-s.conns:
		t.Cleanup(func() { _ = c.Close(); _ = srv.Close() })
		return c, srv
	case <-time.After(portableWait):
		_ = c.Close()
		t.Fatal("fake server never accepted")
		return nil, nil
	}
}

// recorder collects what a registration's callbacks deliver.
type recorder struct {
	mu     sync.Mutex
	data   []byte
	recv   chan struct{}
	closed chan error
}

func newRecorder() *recorder {
	return &recorder{recv: make(chan struct{}, 1024), closed: make(chan error, 4)}
}

func (r *recorder) onRecv(b []byte) {
	r.mu.Lock()
	r.data = append(r.data, b...)
	r.mu.Unlock()
	select {
	case r.recv <- struct{}{}:
	default:
	}
}

func (r *recorder) onClose(err error) { r.closed <- err }

// waitFor blocks until the recorder holds at least n bytes and returns them.
func (r *recorder) waitFor(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.After(portableWait)
	for {
		r.mu.Lock()
		got := append([]byte(nil), r.data...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-r.recv:
		case <-deadline:
			t.Fatalf("onRecv delivered %d bytes (%q), want %d", len(got), got, n)
		}
	}
}

func (r *recorder) waitClose(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.closed:
		return err
	case <-time.After(portableWait):
		t.Fatal("onClose never fired")
		return nil
	}
}

func (r *recorder) noClose(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case err := <-r.closed:
		t.Fatalf("onClose fired early with %v", err)
	case <-time.After(d):
	}
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(portableWait))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("fake server read %d bytes: %v", n, err)
	}
	return buf
}

// connClosed reports whether c has been closed locally: a closed net.Conn
// fails Write with net.ErrClosed, which a live one never does for a dead
// peer before the kernel notices.
func connClosed(c net.Conn) bool {
	_, err := c.Write([]byte{0})
	return errors.Is(err, net.ErrClosed)
}

func TestPortableRegisterDeliversAndWrites(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0)

	client, server := srv.dial(t)
	rec := newRecorder()
	id, err := reg.RegisterNetConn(client, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want a positive identifier", id)
	}

	// Server -> loop: onRecv delivery, across several writes.
	if _, err := server.Write([]byte("+PONG\r\n")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	if _, err := server.Write([]byte("$3\r\nfoo\r\n")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	want := "+PONG\r\n$3\r\nfoo\r\n"
	if got := rec.waitFor(t, len(want)); string(got) != want {
		t.Fatalf("onRecv delivered %q, want %q", got, want)
	}

	// Loop -> server: the write path.
	if err := w.Write(id, []byte("PING\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readN(t, server, 6); string(got) != "PING\r\n" {
		t.Fatalf("server read %q, want PING", got)
	}

	// An id nobody registered.
	if err := w.Write(id+12345, []byte("x")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("Write on unknown id: got %v, want ErrUnknownFD", err)
	}
}

func TestPortablePeerCloseFiresOnCloseAndReleasesConn(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0)

	client, server := srv.dial(t)
	rec := newRecorder()
	id, err := reg.RegisterNetConn(client, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	_ = server.Close()

	if err := rec.waitClose(t); err == nil {
		t.Fatal("onClose(nil) on a peer close: want the read error (io.EOF or a reset)")
	}
	// The conn failed on its own, so nothing will unregister it: the worker
	// must have closed it and dropped its entry.
	deadline := time.Now().Add(portableWait)
	for !connClosed(client) {
		if time.Now().After(deadline) {
			t.Fatal("the conn is still open after the peer closed and onClose fired: it leaks until the garbage collector runs")
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Write(id, []byte("x")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("Write after the peer closed: got %v, want ErrUnknownFD", err)
	}
	if err := w.UnregisterConn(id); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("UnregisterConn after the peer closed: got %v, want ErrUnknownFD", err)
	}
	rec.noClose(t, 50*time.Millisecond) // exactly once
}

func TestPortableUnregisterClosesConnAndFiresOnCloseNil(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0)

	client, server := srv.dial(t)
	rec := newRecorder()
	id, err := reg.RegisterNetConn(client, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	if err := w.UnregisterConn(id); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	if err := rec.waitClose(t); err != nil {
		t.Fatalf("onClose after UnregisterConn: got %v, want nil", err)
	}
	if !connClosed(client) {
		t.Fatal("UnregisterConn left the conn open")
	}
	// The server sees the client leave.
	_ = server.SetReadDeadline(time.Now().Add(portableWait))
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("the server read data from a conn the loop closed")
	}
	if err := w.UnregisterConn(id); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("second UnregisterConn: got %v, want ErrUnknownFD", err)
	}
	if err := w.Write(id, []byte("x")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("Write after UnregisterConn: got %v, want ErrUnknownFD", err)
	}
}

func TestPortableLoopCloseFiresOnCloseForEveryConn(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 2)

	const perWorker = 3
	var recs []*recorder
	var clients []net.Conn
	for n := 0; n < 2; n++ {
		reg := registrar(t, l, n)
		for i := 0; i < perWorker; i++ {
			client, _ := srv.dial(t)
			rec := newRecorder()
			if _, err := reg.RegisterNetConn(client, rec.onRecv, rec.onClose); err != nil {
				t.Fatalf("RegisterNetConn: %v", err)
			}
			recs = append(recs, rec)
			clients = append(clients, client)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, rec := range recs {
		if err := rec.waitClose(t); !errors.Is(err, ErrLoopClosed) {
			t.Fatalf("conn %d: onClose(%v), want ErrLoopClosed", i, err)
		}
		if !connClosed(clients[i]) {
			t.Fatalf("conn %d still open after Loop.Close", i)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// A closed loop takes no new conn and leaves it with the caller.
	client, _ := srv.dial(t)
	if _, err := registrar(t, l, 0).RegisterNetConn(client, nil, nil); !errors.Is(err, ErrLoopClosed) {
		t.Fatalf("RegisterNetConn on a closed loop: got %v, want ErrLoopClosed", err)
	}
	if connClosed(client) {
		t.Fatal("a failed RegisterNetConn closed the caller's conn")
	}
	if err := l.WorkerLoop(0).Write(1, []byte("x")); !errors.Is(err, ErrLoopClosed) {
		t.Fatalf("Write on a closed loop: got %v, want ErrLoopClosed", err)
	}
}

func TestPortableIdentifiers(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0).(*portableOnlyWorker)

	if _, err := reg.RegisterNetConn(nil, nil, nil); err == nil {
		t.Fatal("RegisterNetConn(nil) succeeded")
	}

	seen := map[int]bool{}
	for i := 0; i < 8; i++ {
		client, _ := srv.dial(t)
		id, err := reg.RegisterNetConn(client, func([]byte) {}, func(error) {})
		if err != nil {
			t.Fatalf("RegisterNetConn #%d: %v", i, err)
		}
		if id < netConnIDBase || id > netConnIDMax {
			t.Fatalf("id %d outside [%d, %d]", id, netConnIDBase, netConnIDMax)
		}
		if seen[id] {
			t.Fatalf("id %d handed out twice", id)
		}
		seen[id] = true
	}

	// Wrap-around: the counter at the top of the range starts over at the
	// base and skips an identifier that is still registered.
	var taken int
	for id := range seen {
		if taken == 0 || id < taken {
			taken = id // lowest registered identifier: the base itself
		}
	}
	w.mu.Lock()
	w.nextID = netConnIDMax
	w.mu.Unlock()
	client, _ := srv.dial(t)
	id, err := reg.RegisterNetConn(client, func([]byte) {}, func(error) {})
	if err != nil {
		t.Fatalf("RegisterNetConn after wrap: %v", err)
	}
	if seen[id] {
		t.Fatalf("wrap-around handed out id %d, which is still registered", id)
	}
	if id < netConnIDBase || id > netConnIDMax {
		t.Fatalf("id %d after wrap outside [%d, %d]", id, netConnIDBase, netConnIDMax)
	}
}

// The drivers call back into the loop from the callbacks: pgConn.onClose
// shuts the socket down (shutdownFD) and onRecv writes the startup exchange's
// answers. A worker that held a lock across a callback would deadlock here;
// the timeout is the check.
func TestPortableCallbacksMayCallBackIntoTheLoop(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0)

	client, server := srv.dial(t)
	var id int
	idSet := make(chan struct{})
	echoed := make(chan error, 1)
	closed := make(chan error, 1)
	var err error
	id, err = reg.RegisterNetConn(client,
		func(b []byte) {
			<-idSet
			echoed <- w.Write(id, append([]byte("echo:"), b...))
		},
		func(e error) {
			<-idSet
			// Unregister and Write from inside onClose: both must return.
			_ = w.UnregisterConn(id)
			_ = w.Write(id, []byte("late"))
			closed <- e
		})
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	close(idSet)

	if _, err := server.Write([]byte("hi")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	select {
	case err := <-echoed:
		if err != nil {
			t.Fatalf("Write from onRecv: %v", err)
		}
	case <-time.After(portableWait):
		t.Fatal("onRecv calling Write deadlocked")
	}
	if got := readN(t, server, len("echo:hi")); string(got) != "echo:hi" {
		t.Fatalf("server read %q, want echo:hi", got)
	}

	_ = server.Close()
	select {
	case <-closed:
	case <-time.After(portableWait):
		t.Fatal("onClose calling UnregisterConn and Write deadlocked")
	}
	done := make(chan struct{})
	go func() { _ = l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(portableWait):
		t.Fatal("Loop.Close hung after a conn unregistered itself from onClose")
	}
}

func TestPortableWritesAreSerializedPerConn(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	reg := registrar(t, l, 0)
	w := l.WorkerLoop(0)

	client, server := srv.dial(t)
	id, err := reg.RegisterNetConn(client, func([]byte) {}, func(error) {})
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	const writers, msg = 8, 64 << 10
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(b byte) {
			defer wg.Done()
			buf := make([]byte, msg)
			for j := range buf {
				buf[j] = b
			}
			if err := w.Write(id, buf); err != nil {
				t.Errorf("Write: %v", err)
			}
		}('a' + byte(i))
	}
	got := readN(t, server, writers*msg)
	wg.Wait()
	// Each write must arrive whole: a run of one letter exactly msg long.
	for off := 0; off < len(got); off += msg {
		for j := 1; j < msg; j++ {
			if got[off+j] != got[off] {
				t.Fatalf("writes interleaved at offset %d", off+j)
			}
		}
	}
}

// Loop.Close racing RegisterNetConn: every call either fails with
// ErrLoopClosed and leaves the conn with its caller, or registers a conn that
// Close then closes. Nothing stays open and no reader goroutine outlives the
// Close (the race detector and the -timeout are the checks).
func TestPortableRegisterRacingCloseLeavesNothingOpen(t *testing.T) {
	srv := newLoopbackServer(t)
	for round := 0; round < 20; round++ {
		l, err := NewPortable(2)
		if err != nil {
			t.Fatalf("NewPortable: %v", err)
		}
		const n = 16
		clients := make([]net.Conn, n)
		for i := range clients {
			clients[i], _ = srv.dial(t)
		}
		registered := make([]bool, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				reg := l.WorkerLoop(i % 2).(NetConnRegistrar)
				_, err := reg.RegisterNetConn(clients[i], func([]byte) {}, func(error) {})
				if err == nil {
					registered[i] = true
				} else if !errors.Is(err, ErrLoopClosed) {
					t.Errorf("RegisterNetConn: %v", err)
				}
			}(i)
		}
		close(start)
		closed := make(chan struct{})
		go func() { _ = l.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(portableWait):
			t.Fatalf("round %d: Loop.Close hung: a registration that lost the race started a reader nothing stops", round)
		}
		wg.Wait()
		// A registration that lost the race was refused: its conn is the
		// caller's. One that won is closed by Close, or by the registration
		// itself noticing the loop is gone.
		for i, c := range clients {
			if registered[i] && !connClosed(c) {
				t.Fatalf("round %d: conn %d was registered and is still open after Close", round, i)
			}
			if !registered[i] && connClosed(c) {
				t.Fatalf("round %d: conn %d was refused with ErrLoopClosed but the loop closed it", round, i)
			}
		}
	}
}
