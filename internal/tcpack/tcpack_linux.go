//go:build linux

package tcpack

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var (
	filled atomic.Bool // one-way: set once any socket has reported bytes_acked > 0
	once   sync.Once
)

// probeFn is the host probe; a variable so that a test can stand in for a stack
// that does not fill the field.
var probeFn = probeLoopback

// Note records a count read from a socket: a count above 0 shows the stack fills
// the field, for good.
func Note(acked uint64) {
	if acked > 0 && !filled.Load() {
		filled.Store(true)
	}
}

// Filled reports whether this host's TCP stack fills bytes_acked, so that a count
// of 0 means "the peer has acknowledged nothing". The first call that finds no
// count above 0 yet runs the loopback probe (a few milliseconds at most, once per
// process); a probe that cannot run (no loopback, no listener) answers false:
// the caller then does not close for a 0.
func Filled() bool {
	if filled.Load() {
		return true
	}
	once.Do(func() {
		if probeFn() {
			filled.Store(true)
		}
	})
	return filled.Load()
}

// Usable turns what a read of bytes_acked returned into "is this a count the
// idle clock may judge on": a failed read is not, a count above 0 is (and is
// noted), and a 0 is one only on a stack that fills the field.
func Usable(acked uint64, err error) bool {
	if err != nil {
		return false
	}
	if acked > 0 {
		Note(acked)
		return true
	}
	return Filled()
}

// probeLoopback writes one byte over a loopback connection and waits for the
// acknowledgement to show in the writer's tcp_info.
func probeLoopback() bool {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false
	}
	defer func() { _ = ln.Close() }()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	cl, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = cl.Close() }()
	var sv net.Conn
	select {
	case r := <-ch:
		if r.err != nil {
			return false
		}
		sv = r.c
	case <-time.After(time.Second):
		return false
	}
	defer func() { _ = sv.Close() }()
	_ = sv.SetDeadline(time.Now().Add(time.Second))
	_ = cl.SetDeadline(time.Now().Add(time.Second))
	if _, err := sv.Write([]byte{1}); err != nil {
		return false
	}
	var b [1]byte
	if _, err := cl.Read(b[:]); err != nil {
		return false
	}
	tc, ok := sv.(*net.TCPConn)
	if !ok {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	for range 100 { // the acknowledgement is at most a delayed ACK (40 ms) behind
		var acked uint64
		var gerr error
		if err := rc.Control(func(fd uintptr) {
			var ti *unix.TCPInfo
			ti, gerr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if gerr == nil {
				acked = ti.Bytes_acked
			}
		}); err != nil || gerr != nil {
			return false
		}
		if acked > 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
