// Package bindiag holds the shared bind/listen retry + diagnostic
// helpers used by both the epoll and iouring engines. Linux-only
// (the build tag below excludes everything else); on non-linux the
// engines aren't compiled at all.
//
// Two responsibilities:
//
//   - [BindWithRetry] wraps unix.Bind with bounded exponential-with-
//     jitter retries on transient EADDRINUSE. The race we mitigate is
//     the kernel's per-port bind-table contention when the adaptive
//     engine starts iouring + N epoll-loop sockets concurrently into
//     the same SO_REUSEPORT group; one of the binds occasionally
//     observes EADDRINUSE despite SO_REUSEPORT being set on every
//     member of the group. The condition clears in microseconds.
//
//   - [Format] produces the structured "addr=… SO_REUSEADDR=… …" string
//     attached to bind/listen error messages so a real conflict
//     (post-retry-budget) carries enough state to triage without
//     reproducing.

//go:build linux

package bindiag

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// BindRetries is the maximum number of unix.Bind attempts. Sized so the
// total worst-case sleep budget covers the kernel-bind-table races we've
// observed in cluster matrix runs (sub-ms per race; <100ms across the
// entire SO_REUSEPORT group at startup) while bounding pathological
// kernel state at ~½ second.
const BindRetries = 9

// baseBindDelay is the first retry's base sleep. Jitter doubles each
// attempt, so attempt N sleeps in [base, 2·base) · 2^(N-1).
const baseBindDelay = 250 * time.Microsecond

// BindWithRetry calls unix.Bind on fd with sa, retrying on EADDRINUSE
// up to [BindRetries] times with exponential backoff + per-attempt
// jitter. Non-EADDRINUSE errors short-circuit immediately. The jitter
// matters because all SO_REUSEPORT-group members in a single process
// retry against the same kernel bind table; without it 12+ loops
// sleep+wake in lockstep and re-collide on every attempt.
//
// Sleep schedule (worst-case, no early success):
//
//	attempt 1: [250 µs, 500 µs)
//	attempt 2: [500 µs, 1 ms)
//	...
//	attempt 8: [32 ms, 64 ms)
//	total worst-case across 8 sleeps ≈ 128 ms (rare; typical: 0–1 ms).
func BindWithRetry(fd int, sa unix.Sockaddr) error {
	var err error
	for attempt := range BindRetries {
		if attempt > 0 {
			scale := time.Duration(1 << (attempt - 1))
			// rand.N panics on a zero-or-negative argument; baseBindDelay·scale
			// is always positive and well below the int64 limit (max
			// 32 ms·1 = 32 ms at attempt 8 with the current schedule).
			jitter := time.Duration(rand.Int64N(int64(baseBindDelay * scale)))
			time.Sleep(baseBindDelay*scale + jitter)
		}
		err = unix.Bind(fd, sa)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			return err
		}
	}
	return err
}

// Format returns a one-line diagnostic snapshot useful when bind() or
// listen() fails. Captures the port being bound, the FD's actual
// SO_REUSE* socket-option values (in case a setsockopt didn't take
// effect), the list of other LISTEN sockets currently on the same
// port, and our process's open-FD count. Errors during diagnosis are
// reported inline so a malformed /proc cannot mask the original bind
// failure.
func Format(fd int, sa unix.Sockaddr) string {
	var b strings.Builder

	port := sockaddrPort(sa)
	addr := sockaddrString(sa)
	fmt.Fprintf(&b, "addr=%s", addr)

	if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR); err == nil {
		fmt.Fprintf(&b, " SO_REUSEADDR=%d", v)
	} else {
		fmt.Fprintf(&b, " SO_REUSEADDR=err(%v)", err)
	}
	if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT); err == nil {
		fmt.Fprintf(&b, " SO_REUSEPORT=%d", v)
	} else {
		fmt.Fprintf(&b, " SO_REUSEPORT=err(%v)", err)
	}

	if port > 0 {
		if listeners := procListenersOnPort(port); len(listeners) > 0 {
			fmt.Fprintf(&b, " listeners_on_port=[%s]", strings.Join(listeners, ","))
		} else {
			b.WriteString(" listeners_on_port=[]")
		}
	}

	if n, err := countFDs(); err == nil {
		fmt.Fprintf(&b, " our_open_fds=%d", n)
	}

	return b.String()
}

func sockaddrPort(sa unix.Sockaddr) int {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return v.Port
	case *unix.SockaddrInet6:
		return v.Port
	}
	return 0
}

func sockaddrString(sa unix.Sockaddr) string {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		var buf [21]byte
		b := strconv.AppendUint(buf[:0], uint64(v.Addr[0]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[1]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[2]), 10)
		b = append(b, '.')
		b = strconv.AppendUint(b, uint64(v.Addr[3]), 10)
		b = append(b, ':')
		b = strconv.AppendInt(b, int64(v.Port), 10)
		return string(b)
	case *unix.SockaddrInet6:
		return fmt.Sprintf("[%v]:%d", v.Addr, v.Port)
	}
	return "?"
}

func procListenersOnPort(port int) []string {
	var out []string
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fam := "tcp4"
		if strings.HasSuffix(file, "tcp6") {
			fam = "tcp6"
		}
		f, err := os.Open(file)
		if err != nil {
			out = append(out, fmt.Sprintf("%s=open(%v)", fam, err))
			continue
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 0, 64<<10), 256<<10)
		first := true
		for s.Scan() {
			if first {
				first = false
				continue
			}
			fields := strings.Fields(s.Text())
			if len(fields) < 10 {
				continue
			}
			// fields[1] = local_addr:port (hex), fields[3] = state (hex);
			// 0A = TCP_LISTEN.
			if fields[3] != "0A" {
				continue
			}
			localParts := strings.Split(fields[1], ":")
			if len(localParts) != 2 {
				continue
			}
			p, err := strconv.ParseInt(localParts[1], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			out = append(out, fmt.Sprintf("%s %s uid=%s inode=%s", fam, fields[1], fields[7], fields[9]))
		}
		_ = f.Close()
	}
	return out
}

func countFDs() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}
