//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#936: a GET on a fresh connection to an epoll server that had just
// served a Hijack went unanswered for the whole 60 s conn deadline, twice in
// CI (TestHijackKeepsRequestViews/epoll-async), and three times for the
// sibling TestHijackWithAsyncHandlersOnEpoll at its 5 s deadline. The test's
// own output could not tell a lost request from a starved runner. These
// helpers make it say which, from the kernel's side of the connection rather
// than from timing:
//
//   - LATE: the response arrived after c936StallAfter but inside the deadline.
//     The runner was slow; the request was not lost. Logged, not failed.
//   - Unanswered: the report names the server-side socket of the 4-tuple from
//     /proc/net/tcp (state, rx_queue, inode), the process descriptor that
//     holds it, and every epoll set it is registered in with its event mask
//     (/proc/self/fdinfo), then probes both workers with fresh conns. A
//     request sitting in rx_queue on a descriptor registered for EPOLLIN on a
//     worker that answers the probes is a lost wakeup or a lost read in the
//     engine, whatever the runner's load: no amount of starvation leaves a
//     readable registered socket unread for 60 s while its worker serves
//     other conns.

// c936StallAfter is when a request that has not been answered is first
// examined. It does not shorten the conn deadline; it only takes the
// kernel-side snapshot while the request is still outstanding.
const c936StallAfter = 5 * time.Second

// c936Get is c733Get with the celeris#936 witness. srv may be nil (no engine
// metrics, no worker probes); addr is the server's address for the probes.
func c936Get(t *testing.T, srv *celeris.Server, addr string, conn net.Conn, br *bufio.Reader, path, auth string) (string, error) {
	t.Helper()
	start := time.Now()
	var (
		mu     sync.Mutex
		report string
	)
	stall := time.AfterFunc(c936StallAfter, func() {
		r := c936Report(srv, addr, conn, fmt.Sprintf("%s unanswered after %s", path, c936StallAfter), true)
		mu.Lock()
		report = r
		mu.Unlock()
	})
	body, err := c733Get(conn, br, path, auth)
	elapsed := time.Since(start)
	if !stall.Stop() {
		// The snapshot ran (or is running): wait for it, it holds no lock
		// the request path needs.
		for {
			mu.Lock()
			r := report
			mu.Unlock()
			if r != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	mu.Lock()
	r := report
	mu.Unlock()
	if err == nil {
		if r != "" {
			t.Logf("C936 LATE %s answered after %s (runner stall, the request was not lost); snapshot taken at %s:\n%s", path, elapsed, c936StallAfter, r)
		}
		return body, nil
	}
	final := c936Report(nil, "", conn, fmt.Sprintf("%s failed after %s", path, elapsed), false)
	if r == "" {
		return "", fmt.Errorf("%w\n%s", err, final)
	}
	return "", fmt.Errorf("%w after %s\n%s\n%s", err, elapsed, r, final)
}

// c936Report is the kernel-side account of one outstanding request on conn,
// plus (when srv is set and probe is true) the engine's metrics, a probe of
// both workers and the engine's goroutines.
func c936Report(srv *celeris.Server, addr string, conn net.Conn, what string, probe bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "C936 %s: client %s -> server %s\n", what, conn.LocalAddr(), conn.RemoteAddr())
	b.WriteString(c936SocketReport(conn))
	if srv != nil {
		if info := srv.EngineInfo(); info != nil {
			fmt.Fprintf(&b, "C936 engine=%v metrics=%+v\n", info.Type, info.Metrics)
		}
	}
	if probe && addr != "" {
		b.WriteString(c936ProbeWorkers(addr))
		b.WriteString(c936EngineGoroutines())
	}
	return b.String()
}

// c936SocketReport finds the server side of conn's 4-tuple in /proc/net/tcp
// and says where its bytes are: still in the listen backlog (inode 0, no
// worker accepted it), accepted but unread (rx_queue > 0) or read (rx_queue
// 0), and whether the descriptor that holds it is in an epoll set.
func c936SocketReport(conn net.Conn) string {
	la, ok1 := conn.LocalAddr().(*net.TCPAddr)
	ra, ok2 := conn.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return "C936 socket: not a TCP conn\n"
	}
	raw, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return fmt.Sprintf("C936 socket: /proc/net/tcp: %v\n", err)
	}
	// /proc/net/tcp writes 127.0.0.1 as 0100007F (host order on the
	// little-endian hosts CI runs on) and the port as 4 upper-case hex digits.
	want := fmt.Sprintf("0100007F:%04X 0100007F:%04X", ra.Port, la.Port)
	var b strings.Builder
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[1]+" "+f[2] != want {
			continue
		}
		found = true
		state := c936TCPState(f[3])
		q := strings.SplitN(f[4], ":", 2)
		var txq, rxq int64
		if len(q) == 2 {
			txq, _ = strconv.ParseInt(q[0], 16, 64)
			rxq, _ = strconv.ParseInt(q[1], 16, 64)
		}
		inode := f[9]
		fmt.Fprintf(&b, "C936 server socket %s state=%s tx_queue=%d rx_queue=%d inode=%s\n", want, state, txq, rxq, inode)
		if inode == "0" {
			b.WriteString("C936 verdict-hint: the connection is still in the listen backlog: no worker has accepted it\n")
			continue
		}
		fd, epolls := c936FindSocketFD(inode)
		switch {
		case fd < 0:
			b.WriteString("C936 verdict-hint: no descriptor of this process holds the server socket (accepted, then closed or handed away)\n")
		case len(epolls) == 0:
			fmt.Fprintf(&b, "C936 server fd=%d is in NO epoll set (rx_queue=%d): the engine dropped its registration\n", fd, rxq)
		default:
			fmt.Fprintf(&b, "C936 server fd=%d epoll registrations: %s\n", fd, strings.Join(epolls, "; "))
			if rxq > 0 {
				b.WriteString("C936 verdict-hint: request bytes sit unread on a registered descriptor: the engine did not read them (lost wakeup or lost read)\n")
			} else {
				b.WriteString("C936 verdict-hint: the engine read the request (rx_queue=0) and wrote no response\n")
			}
		}
	}
	if !found {
		fmt.Fprintf(&b, "C936 server socket %s: not in /proc/net/tcp\n", want)
	}
	return b.String()
}

// c936FindSocketFD returns the descriptor of this process that holds the
// socket with the given inode (-1 if none), and for each epoll descriptor of
// this process that has it registered, "epfd=E events=0xM".
func c936FindSocketFD(inode string) (int, []string) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1, nil
	}
	fd := -1
	var epfds []int
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		switch {
		case link == "socket:["+inode+"]":
			fd = n
		case link == "anon_inode:[eventpoll]":
			epfds = append(epfds, n)
		}
	}
	if fd < 0 {
		return -1, nil
	}
	var regs []string
	for _, ep := range epfds {
		raw, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(ep))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			// tfd: <fd> events: <hex mask> data: ...
			if len(f) >= 4 && f[0] == "tfd:" && f[1] == strconv.Itoa(fd) && f[2] == "events:" {
				regs = append(regs, fmt.Sprintf("epfd=%d events=0x%s", ep, f[3]))
			}
		}
	}
	return fd, regs
}

func c936TCPState(hex string) string {
	states := map[string]string{"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2",
		"06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING"}
	if s, ok := states[hex]; ok {
		return s
	}
	return hex
}

// c936ProbeWorkers opens fresh conns while the request is outstanding and
// records which worker answers each (GET /w answers with c.WorkerID()), so a
// wedged worker and a lost request on one conn read differently.
func c936ProbeWorkers(addr string) string {
	var res []string
	for i := 0; i < 6; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			res = append(res, "dial:"+err.Error())
			continue
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		body, err := c733Get(c, bufio.NewReader(c), "/w", "probe")
		switch {
		case err == nil:
			res = append(res, "worker"+body)
		case errors.Is(err, os.ErrDeadlineExceeded):
			res = append(res, "TIMEOUT("+c.LocalAddr().String()+")")
		default:
			res = append(res, "err:"+err.Error())
		}
		_ = c.Close()
	}
	return "C936 probes (fresh conns, 3 s each): " + strings.Join(res, " ") + "\n"
}

// c936EngineGoroutines is the stacks of the goroutines in the engine and
// root packages, which is where a wedged loop or dispatch goroutine shows.
func c936EngineGoroutines() string {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var b strings.Builder
	n := 0
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		if !bytes.Contains(g, []byte("goceleris/celeris/engine")) && !bytes.Contains(g, []byte("goceleris/celeris.(")) {
			continue
		}
		n++
		b.Write(g)
		b.WriteString("\n\n")
	}
	return fmt.Sprintf("C936 engine goroutines (%d):\n%s", n, b.String())
}
