//go:build linux

package epoll

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// bindDiag mirrors engine/iouring.bindDiag — captures the port,
// SO_REUSEADDR/SO_REUSEPORT state, and current listeners on the same
// port when a bind() / listen() call fails. Errors during diagnosis
// are reported inline so they cannot mask the original failure.
func bindDiag(fd int, sa unix.Sockaddr) string {
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
			if len(fields) < 10 || fields[3] != "0A" {
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
