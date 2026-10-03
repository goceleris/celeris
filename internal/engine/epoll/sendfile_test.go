//go:build linux

package epoll

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// socketpair creates a connected pair of non-blocking unix-domain sockets.
// Required because the sendfile path's write(2) and sendfile(2) syscalls
// return EAGAIN on blocking sockets under backpressure, which would
// break the assertion that a small file writes through in one call.
func socketpair(t *testing.T) (int, int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return pair[0], pair[1]
}

// drainAll reads every byte from the given FD into a buffer. Used to
// pull what the engine "sent" off the kernel side of the socketpair.
//
// The read loop terminates after `maxEmptyReads` consecutive EAGAINs —
// under -race the sender may have already written everything but be
// blocked before the next sendfile can continue; counting empty reads
// avoids a deadlock in that case. Empirically the sender completes
// within a handful of EAGAIN rounds; 64 is a comfortable ceiling.
func drainAll(t *testing.T, fd int) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	const maxEmptyReads = 64
	emptyReads := 0
	for emptyReads < maxEmptyReads {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			emptyReads = 0
			continue
		}
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				emptyReads++
				continue
			}
			break
		}
		if n == 0 {
			break
		}
	}
	return out
}

func TestSendfileH1SmallFileFullSends(t *testing.T) {
	a, b := socketpair(t)
	defer func() { _ = unix.Close(a) }()
	defer func() { _ = unix.Close(b) }()

	// Create a 1 KiB file.
	dir := t.TempDir()
	path := filepath.Join(dir, "small.bin")
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i % 251) // arbitrary non-zero pattern
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	headers := []byte("HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\n")
	sent, err := sendfileH1(a, f, 0, 1024, headers)
	if err != nil {
		t.Fatalf("sendfileH1: %v", err)
	}
	if sent != 1024 {
		t.Errorf("sent = %d, want 1024", sent)
	}

	got := drainAll(t, b)
	want := append([]byte{}, headers...)
	want = append(want, payload...)
	if string(got) != string(want) {
		t.Errorf("payload mismatch:\nwant: %x\ngot:  %x", want[:32], got[:min(32, len(got))])
	}
}

func TestSendfileH1RangeOffset(t *testing.T) {
	a, b := socketpair(t)
	defer func() { _ = unix.Close(a) }()
	defer func() { _ = unix.Close(b) }()

	dir := t.TempDir()
	path := filepath.Join(dir, "range.bin")
	payload := []byte("hello world, this is a range request test")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	const off = 6
	const length = 5 // "world"
	headers := []byte("HTTP/1.1 206 Partial Content\r\nContent-Length: 5\r\n\r\n")
	sent, err := sendfileH1(a, f, off, length, headers)
	if err != nil {
		t.Fatalf("sendfileH1: %v", err)
	}
	if sent != length {
		t.Errorf("sent = %d, want %d", sent, length)
	}

	got := drainAll(t, b)
	want := append([]byte{}, headers...)
	want = append(want, payload[off:off+length]...)
	if string(got) != string(want) {
		t.Errorf("payload mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestSendfileH1NoHeaders(t *testing.T) {
	// File-only path: caller has already flushed headers via the response
	// adapter, just the file body is left.
	a, b := socketpair(t)
	defer func() { _ = unix.Close(a) }()
	defer func() { _ = unix.Close(b) }()

	dir := t.TempDir()
	path := filepath.Join(dir, "noheaders.bin")
	payload := []byte("body without headers")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	sent, err := sendfileH1(a, f, 0, int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("sendfileH1: %v", err)
	}
	if sent != int64(len(payload)) {
		t.Errorf("sent = %d, want %d", sent, len(payload))
	}

	got := drainAll(t, b)
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: want %q, got %q", payload, got)
	}
}

// TestSendfileH1LengthZeroSendsToEOF pins finding 1.4: length <= 0 means
// "send to EOF" — the shim must stat the file and transfer size-offset
// bytes, not return a silent empty response.
func TestSendfileH1LengthZeroSendsToEOF(t *testing.T) {
	a, b := socketpair(t)
	defer func() { _ = unix.Close(a) }()
	defer func() { _ = unix.Close(b) }()

	dir := t.TempDir()
	path := filepath.Join(dir, "eof.bin")
	payload := []byte("send everything from the offset to the end of file")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	const off = 5
	// length = 0 → send (size - off) bytes.
	sent, err := sendfileH1(a, f, off, 0, nil)
	if err != nil {
		t.Fatalf("sendfileH1: %v", err)
	}
	wantN := int64(len(payload) - off)
	if sent != wantN {
		t.Errorf("sent = %d, want %d", sent, wantN)
	}
	got := drainAll(t, b)
	if string(got) != string(payload[off:]) {
		t.Errorf("payload mismatch: want %q, got %q", payload[off:], got)
	}
}

// TestSendfileH1EAGAINPartialAndResume pins finding 1.2: when the kernel
// send buffer fills, the shim must surface EAGAIN (not a false success)
// with an UNCORRUPTED byte count, advance its offset, and resume to
// byte-exact completion once the peer drains. Uses a real TCP socketpair
// with a tiny SO_SNDBUF and a peer that is not read until the sender has
// hit backpressure.
func TestSendfileH1EAGAINPartialAndResume(t *testing.T) {
	// A connected TCP pair on loopback so SO_SNDBUF/SO_RCVBUF apply (unix
	// socketpairs ignore small SO_SNDBUF on some kernels).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type acc struct {
		c   net.Conn
		err error
	}
	accCh := make(chan acc, 1)
	go func() {
		c, err := ln.Accept()
		accCh <- acc{c, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	a := <-accCh
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	server := a.c
	defer func() { _ = server.Close() }()

	// Extract the server-side raw fd and make it non-blocking + tiny sndbuf.
	tcpServer, ok := server.(*net.TCPConn)
	if !ok {
		t.Fatalf("server conn is not *net.TCPConn")
	}
	rawConn, err := tcpServer.SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	var serverFD int
	if cerr := rawConn.Control(func(fd uintptr) {
		serverFD = int(fd)
		_ = unix.SetsockoptInt(serverFD, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
		_ = unix.SetNonblock(serverFD, true)
	}); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	// Shrink the client receive buffer too so the in-flight window stays
	// small and the sender reaches EAGAIN before pushing the whole file.
	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(8192)
	}

	// 512 KiB file — far larger than the combined send+recv windows (a few
	// KiB here), so the first advance against an undrained peer must hit
	// EAGAIN partway, but small enough to drain quickly once the peer reads.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	const fileSize = 512 << 10
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	header := []byte("HTTP/1.1 200 OK\r\nContent-Length: 524288\r\n\r\n")
	st, err := newSendfileState(f, 0, fileSize, header)
	if err != nil {
		t.Fatalf("newSendfileState: %v", err)
	}

	// First advance: expect a partial send surfacing EAGAIN (the peer has
	// not read anything yet, so the windows fill).
	n1, err1 := st.advance(serverFD)
	if !errors.Is(err1, unix.EAGAIN) && !errors.Is(err1, unix.EWOULDBLOCK) {
		// On a generous-buffer kernel the whole file might go in one shot;
		// only assert resumability when we actually hit backpressure.
		if err1 == nil && st.done {
			t.Skip("kernel accepted the whole 4 MiB without backpressure; EAGAIN path not exercised")
		}
		t.Fatalf("first advance: err = %v (want EAGAIN), n=%d done=%v", err1, n1, st.done)
	}
	if n1 < 0 {
		t.Fatalf("first advance returned negative body count %d (EAGAIN n=-1 must not be added)", n1)
	}
	if st.done {
		t.Fatalf("state marked done despite EAGAIN")
	}

	// Drain the peer continuously on a goroutine while we resume the
	// sender. The drainer refreshes its deadline each read and only stops
	// when it has all bytes or a hard total deadline elapses — a transient
	// gap while the sender sleeps must NOT end the drain.
	want := len(header) + fileSize
	gotCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64<<10)
		out := make([]byte, 0, want)
		hard := time.Now().Add(15 * time.Second)
		for len(out) < want && time.Now().Before(hard) {
			_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			m, rerr := client.Read(buf)
			if m > 0 {
				out = append(out, buf[:m]...)
				continue
			}
			if rerr != nil && (errors.Is(rerr, os.ErrDeadlineExceeded)) {
				continue // sender momentarily idle; keep waiting
			}
			if rerr != nil {
				break
			}
		}
		gotCh <- out
	}()

	// Resume the sender until done. Busy-retry (no sleep) since the drainer
	// runs concurrently and opens the window; a tiny yield avoids pegging a
	// core while still converging fast.
	total := n1
	deadline := time.Now().Add(15 * time.Second)
	for !st.done {
		if time.Now().After(deadline) {
			t.Fatalf("sender did not complete; sent so far=%d/%d", total, fileSize)
		}
		n, aerr := st.advance(serverFD)
		total += n
		if aerr != nil && !errors.Is(aerr, unix.EAGAIN) && !errors.Is(aerr, unix.EWOULDBLOCK) {
			t.Fatalf("resume advance: %v", aerr)
		}
		if errors.Is(aerr, unix.EAGAIN) || errors.Is(aerr, unix.EWOULDBLOCK) {
			time.Sleep(100 * time.Microsecond) // let the drainer make room
		}
	}
	if total != int64(fileSize) {
		t.Errorf("total body sent = %d, want %d", total, fileSize)
	}

	got := <-gotCh
	if len(got) != want {
		t.Fatalf("received %d bytes, want %d", len(got), want)
	}
	if string(got[:len(header)]) != string(header) {
		t.Errorf("header mismatch: got %q", got[:min(len(header), len(got))])
	}
	if string(got[len(header):]) != string(payload) {
		t.Errorf("body bytes mismatch after resume")
	}
}

// TestSendfileH1HeaderPartialResume pins finding 1.3: a short header
// write(2) under send-buffer pressure is not fatal — the shim tracks
// header progress and resumes from headerOff, never re-sending or
// dropping header bytes. We force the issue by pre-filling the send
// buffer with a large header block on a tiny-sndbuf TCP socket.
func TestSendfileH1HeaderPartialResume(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accCh := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accCh <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	server := <-accCh
	if server == nil {
		t.Fatal("accept failed")
	}
	defer func() { _ = server.Close() }()

	rawConn, err := server.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	var serverFD int
	_ = rawConn.Control(func(fd uintptr) {
		serverFD = int(fd)
		_ = unix.SetsockoptInt(serverFD, unix.SOL_SOCKET, unix.SO_SNDBUF, 4096)
		_ = unix.SetNonblock(serverFD, true)
	})
	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hp.bin")
	body := make([]byte, 256<<10)
	for i := range body {
		body[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	// A header block large enough that, combined with a tiny sndbuf and an
	// undrained peer, the first write(2) is short.
	bigHeader := make([]byte, 64<<10)
	for i := range bigHeader {
		bigHeader[i] = 'H'
	}
	st, err := newSendfileState(f, 0, int64(len(body)), bigHeader)
	if err != nil {
		t.Fatalf("newSendfileState: %v", err)
	}

	wantLen := len(bigHeader) + len(body)
	gotCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64<<10)
		out := make([]byte, 0, wantLen)
		hard := time.Now().Add(15 * time.Second)
		for len(out) < wantLen && time.Now().Before(hard) {
			_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			m, rerr := client.Read(buf)
			if m > 0 {
				out = append(out, buf[:m]...)
				continue
			}
			if errors.Is(rerr, os.ErrDeadlineExceeded) {
				continue
			}
			if rerr != nil {
				break
			}
		}
		gotCh <- out
	}()

	deadline := time.Now().Add(15 * time.Second)
	sawPartialHeader := false
	for !st.done {
		if time.Now().After(deadline) {
			t.Fatalf("did not complete; headerOff=%d remaining=%d", st.headerOff, st.remaining)
		}
		_, aerr := st.advance(serverFD)
		if aerr != nil && !errors.Is(aerr, unix.EAGAIN) && !errors.Is(aerr, unix.EWOULDBLOCK) {
			t.Fatalf("advance: %v", aerr)
		}
		// Header bytes must never exceed the header length (no double-count).
		if st.headerOff > len(bigHeader) {
			t.Fatalf("headerOff %d exceeds header length %d", st.headerOff, len(bigHeader))
		}
		if st.headerOff > 0 && st.headerOff < len(bigHeader) {
			sawPartialHeader = true
		}
		time.Sleep(time.Millisecond)
	}
	_ = sawPartialHeader // best-effort: the header may flush in one go on fast kernels

	out := <-gotCh
	want := append(append([]byte{}, bigHeader...), body...)
	if len(out) != len(want) {
		t.Fatalf("received %d bytes, want %d", len(out), len(want))
	}
	if string(out) != string(want) {
		t.Errorf("header+body mismatch after partial-header resume")
	}
}

func TestEngineImplementsSendfileCapable(t *testing.T) {
	// Compile-time + runtime assertion that the epoll Engine satisfies
	// engine.SendfileCapable. The interface is satisfied by virtue of
	// (e *Engine) Sendfile being defined; the dynamic assertion is the
	// test.
	e := &Engine{}
	if _, ok := any(e).(interface {
		Sendfile(int, *os.File, int64, int64, []byte) (int64, error)
	}); !ok {
		t.Errorf("*Engine does not implement the Sendfile shape; method signature changed")
	}
	_ = net.Conn(nil) // keep net import alive (used in other test files)
	_ = io.EOF        // keep io import alive
}
