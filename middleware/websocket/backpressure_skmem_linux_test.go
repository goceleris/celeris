//go:build linux

package websocket

// The kernel's socket memory, for the WSO-GIVEUP and WSO-CLOSESLOW records
// (celeris#783, celeris#633).
//
// celeris#783 is an x86-only give-up (3 of 5 on a 6.17 runner kernel) in
// which the client's window stays shut while the server's receive queue is
// empty. The hypotheses that fit are all about kernel memory: a receive
// queue pruned or collapsed under memory pressure (the unread bytes the peer
// was ACKed for are dropped), or a TCP memory pool at its pressure line. The
// record therefore carries what ss -tmi prints, without needing ss in the
// image: each end's skmem from getsockopt(SO_MEMINFO) (the same nine
// counters ss shows as skmem:(r,rb,t,tb,f,w,o,bl,d)), the TCP line of
// /proc/net/sockstat against /proc/sys/net/ipv4/tcp_mem, and the TcpExt
// counters that say a queue was pruned or dropped.

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// wsoSkmemFD reads SO_MEMINFO of fd: rmem_alloc, rcvbuf, wmem_alloc, sndbuf,
// fwd_alloc, wmem_queued, optmem, backlog, drops (SK_MEMINFO_*).
func wsoSkmemFD(fd int) (m [unix.SK_MEMINFO_VARS]uint32, err error) {
	n := uint32(unsafe.Sizeof(m))
	_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_SOCKET, unix.SO_MEMINFO,
		uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&n)), 0)
	if e != 0 {
		return m, e
	}
	return m, nil
}

// wsoSkmemString renders the counters the way ss -m does:
// skmem:(r<rmem_alloc>,rb<rcvbuf>,t<wmem_alloc>,tb<sndbuf>,f<fwd_alloc>,
// w<wmem_queued>,o<optmem>,bl<backlog>,d<drops>).
func wsoSkmemString(m [unix.SK_MEMINFO_VARS]uint32) string {
	return fmt.Sprintf("skmem:(r%d,rb%d,t%d,tb%d,f%d,w%d,o%d,bl%d,d%d)",
		m[unix.SK_MEMINFO_RMEM_ALLOC], m[unix.SK_MEMINFO_RCVBUF], m[unix.SK_MEMINFO_WMEM_ALLOC],
		m[unix.SK_MEMINFO_SNDBUF], m[unix.SK_MEMINFO_FWD_ALLOC], m[unix.SK_MEMINFO_WMEM_QUEUED],
		m[unix.SK_MEMINFO_OPTMEM], m[unix.SK_MEMINFO_BACKLOG], m[unix.SK_MEMINFO_DROPS])
}

// wsoSkmemFDString is wsoSkmemString of fd, or why it could not be read.
func wsoSkmemFDString(fd int) string {
	m, err := wsoSkmemFD(fd)
	if err != nil {
		return "skmem:unavailable(" + err.Error() + ")"
	}
	return wsoSkmemString(m)
}

// wsoCliSkmem is the client socket's skmem.
func wsoCliSkmem(c net.Conn) string {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return "skmem:unavailable(not a TCP connection)"
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return "skmem:unavailable(" + err.Error() + ")"
	}
	out := "skmem:unavailable(closed)"
	_ = sc.Control(func(fd uintptr) { out = wsoSkmemFDString(int(fd)) })
	return out
}

// wsoSrvSkmem is the engine's server socket's skmem for the connection the
// client at port holds, read through the engine's own descriptor (read-only,
// see srvSockFD).
func (g *wsoRig) wsoSrvSkmem(port int) string {
	x, ok := g.srvs.Load(port)
	if !ok {
		return "skmem:unavailable(unjoined)"
	}
	fd := g.srvSockFD(x.(*wsoSrv))
	if fd < 0 {
		return "skmem:unavailable(socket gone)"
	}
	return wsoSkmemFDString(fd)
}

// wsoSockstatTCP picks the "TCP:" line of /proc/net/sockstat (inuse, orphan,
// tw, alloc, mem in pages) and the "mem" figure of it. ok is false when the
// line or the figure is missing.
func wsoSockstatTCP(sockstat string) (line string, memPages int64, ok bool) {
	for l := range strings.SplitSeq(sockstat, "\n") {
		if !strings.HasPrefix(l, "TCP:") {
			continue
		}
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "mem" {
				if n, err := strconv.ParseInt(f[i+1], 10, 64); err == nil {
					return strings.TrimSpace(l), n, true
				}
			}
		}
		return strings.TrimSpace(l), 0, false
	}
	return "", 0, false
}

// wsoTCPMemString renders the TCP memory pool against its limits: the mem
// figure of sockstat and tcp_mem (min, pressure, max, all in pages). Where
// tcp_mem cannot be read the reason is printed in its place and the sockstat
// line stands alone. tcp_mem is registered for the initial network namespace
// only, so it is missing in any other one (every container with its own
// netns, Docker Desktop's among them): a verdict on tcp_mem needs a run in
// the host's namespace, such as directly on a runner.
func wsoTCPMemString(sockstat, tcpMem string) string {
	line, mem, ok := wsoSockstatTCP(sockstat)
	if line == "" {
		line = "TCP: absent"
	}
	f := strings.Fields(tcpMem)
	if !ok || len(f) != 3 {
		return fmt.Sprintf("sockstat{%s} tcp_mem{%s}", line, strings.TrimSpace(tcpMem))
	}
	var lim [3]int64
	for i := range lim {
		n, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil {
			return fmt.Sprintf("sockstat{%s} tcp_mem{%s}", line, strings.TrimSpace(tcpMem))
		}
		lim[i] = n
	}
	state := "below the pressure line"
	switch {
	case mem >= lim[2]:
		state = "AT OR ABOVE the hard limit"
	case mem >= lim[1]:
		state = "ABOVE the pressure line (memory pressure)"
	}
	return fmt.Sprintf("sockstat{%s} tcp_mem{min=%d pressure=%d max=%d pages} mem=%d pages: %s",
		line, lim[0], lim[1], lim[2], mem, state)
}

// wsoTCPExtPrune are the TcpExt counters that say a socket queue was pruned,
// collapsed or dropped under memory pressure, or a window was driven to zero.
var wsoTCPExtPrune = map[string]bool{
	"PruneCalled": true, "RcvPruned": true, "OfoPruned": true, "TCPMemoryPressures": true,
	"TCPMemoryPressuresChrono": true, "TCPRcvQDrop": true, "TCPRcvCollapsed": true,
	"TCPZeroWindowDrop": true, "TCPToZeroWindowAdv": true, "TCPFromZeroWindowAdv": true,
	"TCPWantZeroWindowAdv": true, "TCPBacklogDrop": true,
}

// wsoTCPExtCounters picks wsoTCPExtPrune out of /proc/net/netstat (a header
// line of names then a line of values for each of TcpExt and IpExt), in the
// order the kernel prints them.
func wsoTCPExtCounters(netstat string) (names []string, vals map[string]int64) {
	lines := strings.Split(netstat, "\n")
	vals = map[string]int64{}
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "TcpExt:") || !strings.HasPrefix(lines[i+1], "TcpExt:") {
			continue
		}
		hdr, row := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		for j := 1; j < len(hdr) && j < len(row); j++ {
			if !wsoTCPExtPrune[hdr[j]] {
				continue
			}
			if n, err := strconv.ParseInt(row[j], 10, 64); err == nil {
				names = append(names, hdr[j])
				vals[hdr[j]] = n
			}
		}
		break
	}
	return names, vals
}

// wsoTCPExtString renders wsoTCPExtPrune from /proc/net/netstat. The counters
// are netns-wide totals of the whole boot, so a total alone cannot say that
// pruning happened during this run: with base (the same file read when the rig
// started) each is printed as total(+rise since then).
func wsoTCPExtString(netstat, base string) string {
	names, vals := wsoTCPExtCounters(netstat)
	if len(names) == 0 {
		return "tcpext{unavailable}"
	}
	_, was := wsoTCPExtCounters(base)
	out := make([]string, 0, len(names))
	for _, n := range names {
		if b, ok := was[n]; ok {
			out = append(out, fmt.Sprintf("%s=%d(+%d)", n, vals[n], vals[n]-b))
		} else {
			out = append(out, fmt.Sprintf("%s=%d", n, vals[n]))
		}
	}
	return "tcpext{" + strings.Join(out, " ") + "}"
}

func wsoReadProc(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return string(b)
}

// wsoKernelMem renders the host-wide half of the record: TCP pool against
// tcp_mem, and the prune and drop counters with their rise since netstatBase
// (/proc/net/netstat as the rig started; empty prints totals only).
func wsoKernelMem(netstatBase string) string {
	return wsoTCPMemString(wsoReadProc("/proc/net/sockstat"), wsoReadProc("/proc/sys/net/ipv4/tcp_mem")) +
		" " + wsoTCPExtString(wsoReadProc("/proc/net/netstat"), netstatBase)
}
