//go:build linux

package iouring

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// bindDiag returns a one-line diagnostic snapshot useful when a bind()
// call fails with EADDRINUSE despite SO_REUSEADDR + SO_REUSEPORT being
// set. Captures the port being bound, our FD's actual socket-option
// values (in case the setsockopt didn't take effect), and a list of
// other listeners currently bound to the same port on the host.
//
// Format: a single semicolon-separated string suitable for embedding
// in an error wrapper. Errors during diagnosis are reported inline so
// a malformed /proc cannot mask the original bind failure.
func bindDiag(fd int, sa unix.Sockaddr) string {
	var b strings.Builder

	port := sockaddrPort(sa)
	addr := sockaddrString(sa)
	fmt.Fprintf(&b, "addr=%s", addr)

	// Verify the FD actually has SO_REUSEADDR/SO_REUSEPORT set — if a
	// kernel quirk silently dropped one of them, that explains the
	// EADDRINUSE. Both should print as 1.
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

	// Inspect /proc/net/tcp{,6} for sockets in LISTEN state on the same
	// port. Format mirrors `ss -tnlp` minus the privileged PID column
	// (which we can't read without CAP_NET_ADMIN). One line per match.
	if port > 0 {
		if listeners := procListenersOnPort(port); len(listeners) > 0 {
			fmt.Fprintf(&b, " listeners_on_port=[%s]", strings.Join(listeners, ","))
		} else {
			b.WriteString(" listeners_on_port=[]")
		}
	}

	// Per-process FD count under our PID — exhaustion of FDs presents
	// as bind failures in some kernel paths.
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

// procListenersOnPort scans /proc/net/tcp and /proc/net/tcp6 for sockets
// in LISTEN state matching the given port. Returns one descriptor per
// matching row in the form "tcp4|tcp6 <local>:<port> uid=<u> inode=<i>".
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
				continue // header line
			}
			fields := strings.Fields(s.Text())
			if len(fields) < 10 {
				continue
			}
			// fields[1] = local_addr:port (hex), fields[3] = state (hex)
			// State 0A = TCP_LISTEN.
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
