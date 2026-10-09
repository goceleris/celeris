//go:build linux

package stallprobe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The large-response client of P2 and P3: the check celeris's own
// large_response_linux_test.go makes (checkH1Response761), kept to the same
// rule. One request on a new keep-alive connection; the response head and then
// every body byte must arrive, and a client that gets no byte for the idle cap
// (5 s) calls the response lost; the connection must then answer /ping. On top
// of the verdict, every repetition records the longest single wait for bytes
// (so a stall of 2 s, under the cap, is still seen), the time to the response
// head, and the CPU the client socket's receive path ran on (SO_INCOMING_CPU:
// on loopback the sender's softirq delivers, so it is the CPU the server wrote
// from).

type repResult struct {
	Rep       int
	OK        bool
	Err       string
	StartNs   int64
	EndNs     int64
	HeadNs    int64
	MaxIdleNs int64
	InCPU     int
}

type leafResult struct {
	Name    string
	MiB     int
	StartNs int64
	EndNs   int64
	Reps    []repResult
	Stopped bool // stopped early: the stall budget was spent
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>13)
	}
	return b
}

type idleConn struct {
	net.Conn
	c    clk
	idle time.Duration
	n    int
	max  int64
}

func (ic *idleConn) Read(p []byte) (int, error) {
	_ = ic.SetReadDeadline(time.Now().Add(ic.idle))
	t0 := ic.c.now()
	n, err := ic.Conn.Read(p)
	if w := ic.c.now() - t0; w > ic.max {
		ic.max = w
	}
	ic.n += n
	return n, err
}

func describeEnd(err error, idle time.Duration) string {
	var ne net.Error
	switch {
	case err == nil:
		return "no error"
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("no byte for %v, connection still open", idle)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	}
	return err.Error()
}

// fetchLarge does one repetition. buf must hold len(want) bytes.
func fetchLarge(c clk, addr, target string, want, buf []byte, idle time.Duration, rep int) (r repResult) {
	r = repResult{Rep: rep, InCPU: -1, StartNs: c.now()}
	var ic *idleConn
	defer func() {
		r.EndNs = c.now()
		if ic != nil {
			r.MaxIdleNs = ic.max
		}
	}()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		r.Err = "dial: " + err.Error()
		return r
	}
	defer func() { _ = raw.Close() }()
	ic = &idleConn{Conn: raw, c: c, idle: idle}
	br := bufio.NewReaderSize(ic, 64<<10)
	if _, err := fmt.Fprintf(raw, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", target); err != nil {
		r.Err = "write request: " + err.Error()
		return r
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		r.Err = fmt.Sprintf("no response head (%d bytes received, %s)", ic.n, describeEnd(err, idle))
		return r
	}
	r.HeadNs = c.now() - r.StartNs
	n := len(want)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(n) {
		r.Err = fmt.Sprintf("status %d, Content-Length %d, want %d", resp.StatusCode, resp.ContentLength, n)
		return r
	}
	m, err := io.ReadFull(resp.Body, buf[:n])
	if err != nil {
		r.Err = fmt.Sprintf("received %d of %d body bytes (%d bytes in all), then %s", m, n, ic.n, describeEnd(err, idle))
		return r
	}
	if !bytes.Equal(buf[:n], want) {
		i := 0
		for i < n && buf[i] == want[i] {
			i++
		}
		r.Err = fmt.Sprintf("all bytes arrived but differ from byte %d on", i)
		return r
	}
	if k, err := resp.Body.Read(make([]byte, 1)); k != 0 || !errors.Is(err, io.EOF) {
		r.Err = fmt.Sprintf("the body did not end after %d bytes (%d more, %v)", n, k, err)
		return r
	}
	_ = resp.Body.Close()
	if tc, ok := raw.(*net.TCPConn); ok {
		if sc, err := tc.SyscallConn(); err == nil {
			_ = sc.Control(func(fd uintptr) {
				if v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_INCOMING_CPU); err == nil {
					r.InCPU = v
				}
			})
		}
	}
	if _, err := io.WriteString(raw, "GET /ping HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		r.Err = "the connection took no next request: " + err.Error()
		return r
	}
	next, err := http.ReadResponse(br, nil)
	if err != nil {
		r.Err = "no answer to the next request on the connection (" + describeEnd(err, idle) + ")"
		return r
	}
	b, err := io.ReadAll(next.Body)
	_ = next.Body.Close()
	if err != nil || next.StatusCode != http.StatusOK || string(b) != "ok" {
		r.Err = fmt.Sprintf("next request answered %d %q (%v)", next.StatusCode, b, err)
		return r
	}
	r.OK = true
	return r
}

// runLeaf makes reps repetitions of one request, stopping early once maxStalls
// of them failed (a stalled repetition costs the whole idle cap).
func runLeaf(c clk, name string, mib int, addr, target string, want, buf []byte, reps, maxStalls int, idle time.Duration) leafResult {
	l := leafResult{Name: name, MiB: mib, StartNs: c.now()}
	failed := 0
	for i := 1; i <= reps; i++ {
		r := fetchLarge(c, addr, target, want, buf, idle, i)
		l.Reps = append(l.Reps, r)
		if !r.OK {
			failed++
			if failed >= maxStalls {
				l.Stopped = i < reps
				break
			}
		}
	}
	l.EndNs = c.now()
	return l
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// reportLeaf logs one leaf's repetitions and fails t when any failed.
func reportLeaf(t *testing.T, tp topo, l leafResult, wd wdResult) {
	t.Helper()
	var tot, head, idle []int64
	failed := 0
	cpus := map[int]int{}
	for _, r := range l.Reps {
		if r.OK {
			tot = append(tot, r.EndNs-r.StartNs)
		} else {
			failed++
		}
		head = append(head, r.HeadNs)
		idle = append(idle, r.MaxIdleNs)
		cpus[r.InCPU]++
	}
	var mx = func(v []int64) (lo, hi int64) {
		lo, hi = 1<<62, 0
		for _, x := range v {
			lo, hi = min(lo, x), max(hi, x)
		}
		return
	}
	tlo, thi := mx(tot)
	_, hhi := mx(head)
	_, ihi := mx(idle)
	var ids []int
	for k := range cpus {
		ids = append(ids, k)
	}
	sort.Ints(ids)
	var cs []string
	for _, k := range ids {
		if k < 0 {
			cs = append(cs, fmt.Sprintf("?x%d", cpus[k]))
			continue
		}
		cs = append(cs, fmt.Sprintf("cpu%d(%s)x%d", k, tp.CPU[k].class(), cpus[k]))
	}
	wmax, w50, w500 := wdWithin(wd, l.StartNs, l.EndNs)
	logf(t, "reps run %d, ok %d, failed %d%s; ok-rep total ms min/med/max %s/%s/%s; response head ms med/max %s/%s; longest wait for bytes ms med/max %s/%s; receive-path CPU: %s\nprocess watchdog (1 ms sleeps) during the leaf: max oversleep %s ms, >=50 ms: %d, >=500 ms: %d",
		len(l.Reps), len(l.Reps)-failed, failed, map[bool]string{true: " (stopped early: stall budget spent)", false: ""}[l.Stopped],
		msN2(tot, tlo), ms(median(tot)), ms(thi), ms(median(head)), ms(hhi), ms(median(idle)), ms(ihi), strings.Join(cs, " "), ms(wmax), w50, w500)
	for _, r := range l.Reps {
		if !r.OK {
			t.Errorf("rep %d failed after %s ms (longest wait %s ms, receive-path CPU %d): %s", r.Rep, ms(r.EndNs-r.StartNs), ms(r.MaxIdleNs), r.InCPU, r.Err)
		}
	}
}

func msN2(v []int64, lo int64) string {
	if len(v) == 0 {
		return "-"
	}
	return ms(lo)
}
