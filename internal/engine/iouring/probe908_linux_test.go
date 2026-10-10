//go:build linux

package iouring

// celeris#988 diagnostic probe, test side (probe branch only, not for merge).
// The snapshots print on failure always, and at the half-way point and at
// the 2 s / 5 s marks of a wait only with CELERIS_PROBE_908=1.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// adoptCarryFD908 is adoptCarry908 that also returns the adopted descriptor's
// number: the engine owns the descriptor afterwards, so the number is only
// ever used for read-only getsockopt/ioctl, checked against the peer address
// (probe908SockSnap) because it can be reused after the engine closes it.
func adoptCarryFD908(t *testing.T, adopt func(int, engine.Carryover) error, carried string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	client, fd := adoptable543(t)
	if err := adopt(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String(), Buffered: []byte(carried)}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn with a carry: %v", err)
	}
	return client, bufio.NewReader(client), fd
}

// probe908T takes snapshots of both ends of the pair for one test.
type probe908T struct {
	t      *testing.T
	client net.Conn
	srvFD  int // the adopted descriptor's number, or -1 (the recv-path control)
	start  time.Time
	read   int64 // body bytes the test has read so far
	half   bool
}

func newProbe908(t *testing.T, client net.Conn, srvFD int) *probe908T {
	return &probe908T{t: t, client: client, srvFD: srvFD, start: time.Now()}
}

func (p *probe908T) clientSnap() string {
	rc, err := p.client.(*net.TCPConn).SyscallConn()
	if err != nil {
		return "client syscallconn: " + err.Error()
	}
	s := "no fd"
	_ = rc.Control(func(fd uintptr) { s = probe908SockSnap(int(fd), "") })
	return s
}

// snap logs the client's socket, the server-side socket (read from this
// goroutine, independent of the worker) and, with the probe on, the engine's
// own state, answered by the worker thread or reported as unanswered.
func (p *probe908T) snap(label string) {
	if p == nil {
		return
	}
	p.t.Helper()
	p.t.Logf("PROBE908 test t=+%s read=%d [%s]\n  client: %s", time.Since(p.start).Round(time.Millisecond), p.read, label, p.clientSnap())
	if p.srvFD >= 0 {
		p.t.Logf("  server: %s", probe908SockSnap(p.srvFD, p.client.LocalAddr().String()))
		if eng := probe908Ask(label, p.client.LocalAddr().String(), 3*time.Second); eng != "" {
			p.t.Logf("  %s", strings.ReplaceAll(eng, "\n", "\n  "))
		}
	}
}

// failf snapshots and fails the test.
func (p *probe908T) failf(format string, a ...any) {
	p.t.Helper()
	p.snap("FAIL: " + fmt.Sprintf(format, a...))
	p.t.Fatalf(format, a...)
}

// halfReader reads from r and takes the half-way snapshot once.
type halfReader struct {
	p     *probe908T
	r     io.Reader
	total int64
}

func (h *halfReader) Read(b []byte) (int, error) {
	n, err := h.r.Read(b)
	h.p.read += int64(n)
	if !h.p.half && probe908On && h.p.read >= h.total/2 {
		h.p.half = true
		h.p.snap(fmt.Sprintf("HALF-WAY (%d of %d)", h.p.read, h.total))
	}
	return n, err
}

func (p *probe908T) halfReader(r io.Reader, total int64) io.Reader {
	if p == nil {
		return r
	}
	return &halfReader{p: p, r: r, total: total}
}
