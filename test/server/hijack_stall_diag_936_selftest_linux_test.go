//go:build linux

package celeris_test

import (
	"bufio"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Controls for the celeris#936 witness itself (hijack_stall_diag_936_linux_test.go):
// what it says must not be stronger than what it can see.

// finish must return when the snapshot ran and produced nothing, and must not
// wait for a snapshot that never started.
func TestC936LookFinish(t *testing.T) {
	cases := []struct {
		name  string
		after time.Duration
		take  func() string
		wait  time.Duration // finish is called this long after the watch starts
		want  string
	}{
		{"ran-empty", time.Millisecond, func() string { return "" }, 100 * time.Millisecond, ""},
		{"ran-slow-empty", time.Millisecond, func() string { time.Sleep(50 * time.Millisecond); return "" }, 10 * time.Millisecond, ""},
		{"ran-text", time.Millisecond, func() string { return "snap" }, 100 * time.Millisecond, "snap"},
		{"never-ran", time.Hour, func() string { t.Error("take must not run"); return "x" }, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := c936Watch(tc.after, tc.take)
			time.Sleep(tc.wait)
			got := make(chan string, 1)
			go func() { got <- l.finish() }()
			select {
			case r := <-got:
				if r != tc.want {
					t.Fatalf("finish = %q, want %q", r, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("finish did not return")
			}
		})
	}
}

// The deadline label belongs to a deadline only: an immediate EOF or reset is
// not "unanswered at the deadline".
func TestC936Cause(t *testing.T) {
	dl := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	if got := c936Cause(dl, "/w", 5*time.Second); !strings.Contains(got, "unanswered at the 5s deadline") {
		t.Errorf("deadline error labelled %q", got)
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET} {
		got := c936Cause(err, "/w", 5*time.Second)
		if strings.Contains(got, "deadline") || !strings.Contains(got, "failed") {
			t.Errorf("%v labelled %q: must not claim a deadline", err, got)
		}
	}
}

// A request the server has read, on a descriptor registered for EPOLLIN, with
// nothing sent back: the kernel's account (tx_queue=0, rx_queue=0) cannot tell
// "no response was produced" from "produced and never flushed", and the hint
// must not claim the former.
func TestC936HintReadNoResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	read := make(chan error, 1)
	srvConn := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			read <- err
			return
		}
		srvConn <- c
		_, err = bufio.NewReader(c).ReadString('\n') // the request line is consumed
		read <- err
	}()
	cl, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	sc := <-srvConn
	defer func() { _ = sc.Close() }()
	// Register the server descriptor for EPOLLIN in an epoll set of this
	// process, as the engine does for a conn it serves.
	raw, err := sc.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(epfd) }()
	var ctlErr error
	if err := raw.Control(func(fd uintptr) {
		ctlErr = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, int(fd), &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)})
	}); err != nil || ctlErr != nil {
		t.Fatalf("epoll_ctl: %v %v", err, ctlErr)
	}
	if _, err := io.WriteString(cl, "GET /w HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	var rep string
	for i := 0; i < 100; i++ { // rx_queue reaches 0 once the bufio read returned, but allow the headers' tail
		rep = c936SocketReport(cl)
		if strings.Contains(rep, "verdict-hint") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(rep, "state=ESTABLISHED") || !strings.Contains(rep, "tx_queue=0") {
		t.Fatalf("report does not describe the server socket:\n%s", rep)
	}
	if !strings.Contains(rep, "no response bytes reached this socket") || !strings.Contains(rep, "user-space") {
		t.Errorf("hint does not scope its claim to the socket:\n%s", rep)
	}
	if strings.Contains(rep, "wrote no response") {
		t.Errorf("hint claims the engine produced no response:\n%s", rep)
	}
}
