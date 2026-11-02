//go:build linux

package sockopts

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// SO_BUSY_POLL is opt-in (celeris#1036): ApplyFD and ApplyConn
// set it, and ask for SO_PREFER_BUSY_POLL with it, only for a
// positive SOBusyPoll, and leave the socket as the kernel made it otherwise.

// acceptedPair returns a connected pair of loopback TCP sockets; the server
// side is the accepted one.
func acceptedPair(t *testing.T) (server *net.TCPConn, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := <-ch
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	t.Cleanup(func() { _ = r.c.Close() })
	return r.c.(*net.TCPConn), client
}

func readOpt(t *testing.T, c *net.TCPConn, opt int) int {
	t.Helper()
	v, err := readOptErr(t, c, opt)
	if err != nil {
		t.Fatalf("getsockopt %d: %v", opt, err)
	}
	return v
}

func readOptErr(t *testing.T, c *net.TCPConn, opt int) (int, error) {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var v int
	var gerr error
	if err := raw.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, opt) }); err != nil {
		t.Fatalf("Control: %v", err)
	}
	return v, gerr
}

func applyFDOn(t *testing.T, c *net.TCPConn, opts Options) error {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var aerr error
	if err := raw.Control(func(fd uintptr) { aerr = ApplyFD(int(fd), opts) }); err != nil {
		t.Fatalf("Control: %v", err)
	}
	return aerr
}

func TestApplyFDBusyPollIsOptIn(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, tc := range []struct {
		name string
		opts Options
		want int
	}{
		{"zero options", Options{}, base},
		{"every other option, no busy poll", Options{TCPNoDelay: true, TCPQuickAck: true, RecvBuf: 65536, SendBuf: 65536}, base},
		{"sub-microsecond is not a budget", Options{SOBusyPoll: 500 * time.Nanosecond}, base},
		{"37 microseconds", Options{SOBusyPoll: 37 * time.Microsecond}, 37},
		{"50 microseconds", Options{SOBusyPoll: 50 * time.Microsecond}, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := acceptedPair(t)
			if err := applyFDOn(t, srv, tc.opts); err != nil {
				t.Fatalf("ApplyFD: %v", err)
			}
			if got := readOpt(t, srv, unix.SO_BUSY_POLL); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.want, base)
			}
		})
	}
}

// TestApplyFDPreferAndBudgetOnlyWithBusyPoll: with busy poll off, the two
// companion options are never touched. With it on they are tried (best
// effort: SO_PREFER_BUSY_POLL needs CAP_NET_ADMIN, so what they read depends
// on the process), which is logged and, when the process may set it, asserted.
func TestApplyFDPreferAndBudgetOnlyWithBusyPoll(t *testing.T) {
	// SO_BUSY_POLL_BUDGET is write-only on some kernels (getsockopt answers
	// ENOPROTOOPT): its readings are compared only where it can be read.
	fresh, _ := acceptedPair(t)
	freshPrefer := readOpt(t, fresh, sockoptstest.SOPreferBusyPoll)
	freshBudget, budgetErr := readOptErr(t, fresh, sockoptstest.SOBusyPollBudget)

	srv, _ := acceptedPair(t)
	if err := applyFDOn(t, srv, Options{TCPQuickAck: true}); err != nil {
		t.Fatalf("ApplyFD: %v", err)
	}
	if got := readOpt(t, srv, sockoptstest.SOPreferBusyPoll); got != freshPrefer {
		t.Errorf("busy poll off: SO_PREFER_BUSY_POLL = %d, want the untouched %d", got, freshPrefer)
	}
	if got, err := readOptErr(t, srv, sockoptstest.SOBusyPollBudget); err == nil && got != freshBudget {
		t.Errorf("busy poll off: SO_BUSY_POLL_BUDGET = %d, want the untouched %d", got, freshBudget)
	}

	srv, _ = acceptedPair(t)
	if err := applyFDOn(t, srv, Options{SOBusyPoll: 50 * time.Microsecond}); err != nil {
		t.Fatalf("ApplyFD: %v", err)
	}
	prefer := readOpt(t, srv, sockoptstest.SOPreferBusyPoll)
	budget, _ := readOptErr(t, srv, sockoptstest.SOBusyPollBudget)
	t.Logf("busy poll on: SO_PREFER_BUSY_POLL=%d (untouched %d), SO_BUSY_POLL_BUDGET=%d (untouched %d, readable: %v); euid=%d",
		prefer, freshPrefer, budget, freshBudget, budgetErr == nil, unix.Geteuid())
	// SO_PREFER_BUSY_POLL is refused without CAP_NET_ADMIN, which an
	// ordinary CI run does not have: the assertion is made where the process
	// can set it on a socket of its own, and is a no-op elsewhere.
	if canSetPrefer(t) && prefer != 1 {
		t.Errorf("busy poll on, with CAP_NET_ADMIN: SO_PREFER_BUSY_POLL = %d, want 1", prefer)
	}
	if !canSetPrefer(t) && prefer != freshPrefer {
		t.Errorf("busy poll on, without CAP_NET_ADMIN: SO_PREFER_BUSY_POLL = %d, want the untouched %d", prefer, freshPrefer)
	}
}

// TestApplyFDRefusedBusyPollKeepsTheBufferSizes: a kernel that refuses
// SO_BUSY_POLL (one built without CONFIG_NET_RX_BUSY_POLL, or one that will
// not raise it for this process) must not take SO_RCVBUF and SO_SNDBUF with
// it. The refusal is forced portably: the option is a C int, so a budget of
// 2^31 microseconds reaches the kernel as a negative one and gets EINVAL.
func TestApplyFDRefusedBusyPollKeepsTheBufferSizes(t *testing.T) {
	srv, _ := acceptedPair(t)
	err := applyFDOn(t, srv, Options{
		SOBusyPoll: (1 << 31) * time.Microsecond,
		RecvBuf:    50000,
		SendBuf:    50000,
	})
	if err == nil {
		t.Fatal("premise: a budget of 2^31 microseconds was not refused by the kernel")
	}
	// The kernel doubles the value it is given (and 50000 is under its
	// default rmem_max and wmem_max, and away from the default 128 KiB
	// receive buffer a new socket reads, which 65536 would equal).
	if got := readOpt(t, srv, unix.SO_RCVBUF); got != 2*50000 {
		t.Errorf("SO_RCVBUF = %d after a refused SO_BUSY_POLL, want %d: the busy poll error ended ApplyFD before the buffer sizes", got, 2*50000)
	}
	if got := readOpt(t, srv, unix.SO_SNDBUF); got != 2*50000 {
		t.Errorf("SO_SNDBUF = %d after a refused SO_BUSY_POLL, want %d", got, 2*50000)
	}
}

func TestApplyConnBusyPollIsOptIn(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, tc := range []struct {
		name string
		d    time.Duration
		want int
	}{
		{"default", 0, base},
		{"37 microseconds", 37 * time.Microsecond, 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := acceptedPair(t)
			if err := ApplyConn(srv, Options{TCPNoDelay: true, SOBusyPoll: tc.d}); err != nil {
				t.Fatalf("ApplyConn: %v", err)
			}
			if got := readOpt(t, srv, unix.SO_BUSY_POLL); got != tc.want {
				t.Errorf("SO_BUSY_POLL = %d, want %d (baseline %d)", got, tc.want, base)
			}
		})
	}
}

// canSetPrefer reports whether this process may set SO_PREFER_BUSY_POLL
// (CAP_NET_ADMIN), by trying it on a socket of its own.
func canSetPrefer(t *testing.T) bool {
	t.Helper()
	srv, _ := acceptedPair(t)
	raw, err := srv.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, sockoptstest.SOPreferBusyPoll, 1)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	return serr == nil
}
