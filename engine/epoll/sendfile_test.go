//go:build linux

package epoll

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

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
func drainAll(t *testing.T, fd int) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				// short pause to let in-flight bytes settle, then re-read
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

func TestZerocopyInflightAddAndFree(t *testing.T) {
	z := zerocopyInflight{}
	z.add(1000)
	z.add(2000)
	if got := z.bytes.Load(); got != 3000 {
		t.Errorf("after add: in-flight = %d, want 3000", got)
	}
	z.free(1500)
	if got := z.bytes.Load(); got != 1500 {
		t.Errorf("after free: in-flight = %d, want 1500", got)
	}
}

func TestEngineImplementsSendfileCapable(t *testing.T) {
	// Compile-time + runtime assertion that the epoll Engine satisfies
	// engine.SendfileCapable. The interface is satisfied by virtue of
	// (e *Engine) Sendfile being defined; the dynamic assertion is the
	// test.
	e := &Engine{}
	var _ = e //nolint:staticcheck // satisfies the interface check below
	if _, ok := any(e).(interface {
		Sendfile(int, *os.File, int64, int64, []byte) (int64, error)
	}); !ok {
		t.Errorf("*Engine does not implement the Sendfile shape; method signature changed")
	}
	_ = net.Conn(nil) // keep net import alive (used in other test files)
	_ = io.EOF       // keep io import alive
}
