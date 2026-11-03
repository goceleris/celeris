//go:build linux

package iouring

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dialTimeout896 is the timeout of the dials of the 896 tests that
// dialWatch896 watches.
const dialTimeout896 = 2 * time.Second

// dialWatch896 watches what the kernel and the test process do while a test
// dials addr, for the failure message of a dial that times out (celeris#1229:
// 2 s with no answer from an engine that is listening). It reads /proc only,
// ignores every error, and never blocks: report is the whole of its cost.
//
// A dial that fails is one of two things. Either the handshake did not
// complete (the SYN or the SYN-ACK was dropped or not processed) or it
// completed and the test did not see it for 2 s. Go closes the socket of a
// failed dial before DialTimeout returns, so the client's row is gone when
// report reads /proc and the watch takes its own snapshots while the dial is
// still going (near): a client in SYN_SENT there, with the retransmit count
// of its row, is a SYN nobody answered. After the timeout (after) the rows say
// what the close left: no row but LISTEN, with nothing in SYN_RECV, is a
// listener that never saw the SYN; a client in FIN_WAIT or TIME_WAIT, a
// server-side ESTABLISHED, CLOSE_WAIT or SYN_RECV row, or a LISTEN row with
// rx_queue above 0, is a handshake that got through. For a LISTEN socket
// tx_queue is always 0 and rx_queue is its accept queue alone: a connection
// TCP_DEFER_ACCEPT holds back is in the request queue and is not counted. The
// counters say where a refused SYN was counted, and the largest gap between
// the ticks of a 10 ms ticker says whether the test process (or the VM) was
// not running for that long.
type dialWatch896 struct {
	port     int
	now      func() time.Time        // time.Now; a test replaces it
	t0       time.Time               // when the watch began
	rows     func(port int) []string // tcpRows896; a test replaces it
	backlog  func() int64            // softnetBacklog896
	timeout  time.Duration
	before   procCounters896
	stop     chan struct{}
	finished chan struct{}
	gaps     gapTracker896

	// written by the watch goroutine, read after finished is closed
	near     []string
	nearAt   time.Duration
	backlogN int64 // softnet backlog length summed over the CPUs, at near
}

// gapTracker896 records the largest gap between two ticks. It takes the time
// as an argument: the value a Go ticker delivers is the time the tick was
// scheduled for, so after a stall the first late tick carries a time 10 ms
// after the last and the gap would show as 10 ms.
type gapTracker896 struct {
	last time.Time
	max  atomic.Int64
}

func (g *gapTracker896) tick(now time.Time) {
	if gap := int64(now.Sub(g.last)); gap > g.max.Load() {
		g.max.Store(gap)
	}
	g.last = now
}

// procCounters896 are the counters a failed dial is compared on, read from
// /proc at a moment.
type procCounters896 struct {
	ext     map[string]int64 // TcpExt and IpExt of /proc/net/netstat, by name
	tcp     map[string]int64 // Tcp of /proc/net/snmp
	dropped int64            // softnet_stat column 1, summed over the CPUs
	squeeze int64            // softnet_stat column 2, summed over the CPUs
}

// dialCounters896 name the counters whose deltas are reported, in the order
// they are printed: the TcpExt and IpExt ones of /proc/net/netstat first, the
// Tcp ones of /proc/net/snmp after.
var (
	dialExtCounters896 = []string{"ListenDrops", "ListenOverflows", "TCPReqQFullDrop", "TCPReqQFullDoCookies",
		"TCPDeferAcceptDrop", "TCPBacklogDrop", "TCPMemoryPressures", "PFMemallocDrop", "InNoRoutes"}
	dialTCPCounters896 = []string{"AttemptFails", "EstabResets", "RetransSegs", "InSegs", "InErrs"}
)

func readProcCounters896() procCounters896 {
	c := procCounters896{ext: map[string]int64{}, tcp: map[string]int64{}}
	if b, err := os.ReadFile("/proc/net/netstat"); err == nil {
		c.ext = parseProcPairs896(string(b), "TcpExt:", "IpExt:")
	}
	if b, err := os.ReadFile("/proc/net/snmp"); err == nil {
		c.tcp = parseProcPairs896(string(b), "Tcp:")
	}
	if b, err := os.ReadFile("/proc/net/softnet_stat"); err == nil {
		c.dropped = softnetSum896(string(b), softnetDroppedCol896)
		c.squeeze = softnetSum896(string(b), softnetSqueezeCol896)
	}
	return c
}

// deltas896 prints now - before for the named counters that exist, as
// "Name+n".
func deltas896(names []string, before, now map[string]int64) []string {
	var out []string
	for _, k := range names {
		if v, ok := now[k]; ok {
			out = append(out, fmt.Sprintf("%s+%d", k, v-before[k]))
		}
	}
	return out
}

func watchDial896(addr string, timeout time.Duration) *dialWatch896 {
	return watchDialClock896(addr, timeout, time.Now)
}

func watchDialClock896(addr string, timeout time.Duration, now func() time.Time) *dialWatch896 {
	w := &dialWatch896{now: now, timeout: timeout, rows: tcpRows896, backlog: softnetBacklog896,
		stop: make(chan struct{}), finished: make(chan struct{})}
	if _, ps, err := net.SplitHostPort(addr); err == nil {
		w.port, _ = strconv.Atoi(ps)
	}
	w.before = readProcCounters896()
	started := make(chan struct{})
	go func() {
		defer close(w.finished)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		snap := time.NewTicker(250 * time.Millisecond)
		defer snap.Stop()
		w.t0 = now()
		w.gaps.last = w.t0
		close(started)
		for {
			select {
			case <-w.stop:
				w.onStop()
				return
			case <-tk.C:
				w.onTick()
			case <-snap.C:
				w.onSnap()
			}
		}
	}()
	<-started
	return w
}

// onTick records the gap since the last tick, with the clock and not with the
// value the ticker delivered.
func (w *dialWatch896) onTick() { w.gaps.tick(w.now()) }

// onStop records the stall that ends the dial: the gap since the last tick.
func (w *dialWatch896) onStop() { w.gaps.tick(w.now()) }

// onSnap keeps, from half the timeout on, the last snapshot of the port's rows
// that has a row: the client's socket is closed before the dial returns.
func (w *dialWatch896) onSnap() {
	if at := w.now().Sub(w.t0); at >= w.timeout/2 {
		if rows := w.rows(w.port); len(rows) > 0 {
			w.near, w.nearAt = rows, at
			w.backlogN = w.backlog()
		}
	}
}

// end stops the watch; a watch whose dial succeeded calls only this.
func (w *dialWatch896) end() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.finished
}

// maxRows896 bounds the rows one snapshot prints.
const maxRows896 = 8

func boundRows896(rows []string) string {
	if len(rows) == 0 {
		return "none"
	}
	if len(rows) > maxRows896 {
		return strings.Join(rows[:maxRows896], "; ") + fmt.Sprintf("; (+%d more)", len(rows)-maxRows896)
	}
	return strings.Join(rows, "; ")
}

// report ends the watch and describes the dial that just failed.
func (w *dialWatch896) report() string {
	w.end()
	now := readProcCounters896()
	d := deltas896(dialExtCounters896, w.before.ext, now.ext)
	d = append(d, deltas896(dialTCPCounters896, w.before.tcp, now.tcp)...)
	d = append(d, fmt.Sprintf("softnet_drop+%d softnet_squeeze+%d", now.dropped-w.before.dropped, now.squeeze-w.before.squeeze))
	near := "no snapshot"
	if w.near != nil {
		near = fmt.Sprintf("at %v: [%s] syn_recv=%d softnet_backlog_len=%d", w.nearAt.Round(10*time.Millisecond),
			boundRows896(w.near), countState896(w.near, "SYN_RECV"), w.backlogN)
	}
	return fmt.Sprintf("sockets on port %d: near the deadline %s; after the timeout [%s]; counters: %s; "+
		"largest gap between 10 ms ticks: %v", w.port, near, boundRows896(tcpRows896(w.port)), strings.Join(d, " "),
		time.Duration(w.gaps.max.Load()).Round(time.Millisecond))
}

// countState896 counts the rows of tcpRows896 in the named state.
func countState896(rows []string, state string) int {
	n := 0
	for _, r := range rows {
		if strings.Contains(r, " "+state+" ") {
			n++
		}
	}
	return n
}

// tcpStates896 names the st column of /proc/net/tcp (the kernel prints a
// request socket as 03, SYN_RECV).
var tcpStates896 = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2",
	"06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING",
	"0C": "NEW_SYN_RECV",
}

// tcpRows896 returns every row of /proc/net/tcp and tcp6 whose local or
// remote port is port, in any state, with its state named.
func tcpRows896(port int) []string {
	var rows []string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			rows = append(rows, tcpRowsOf896(filepath.Base(f), string(b), port)...)
		}
	}
	return rows
}

// tcpRowsOf896 is tcpRows896 on the text of one file, tag its name.
func tcpRowsOf896(tag, text string, port int) []string {
	var rows []string
	want := fmt.Sprintf(":%04X", port)
	for _, ln := range strings.Split(text, "\n") {
		fl := strings.Fields(ln)
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt ...
		if len(fl) < 5 || fl[0] == "sl" || (!strings.HasSuffix(fl[1], want) && !strings.HasSuffix(fl[2], want)) {
			continue
		}
		st := tcpStates896[fl[3]]
		if st == "" {
			st = fl[3]
		}
		row := fmt.Sprintf("%s %s>%s %s q(tx:rx)=%s", tag, fl[1], fl[2], st, fl[4])
		if len(fl) > 6 { // retrnsmt, hex: the SYN (or SYN-ACK) was sent again
			if n, err := strconv.ParseInt(fl[6], 16, 64); err == nil && n != 0 {
				row += fmt.Sprintf(" retrnsmt=%d", n)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// parseProcPairs896 parses the pairs of lines of /proc/net/netstat and
// /proc/net/snmp: "Prefix: Name Name ..." followed by "Prefix: n n ...". The
// first pair of each prefix is read; a name present under two prefixes is
// taken from the later one asked for.
func parseProcPairs896(text string, prefixes ...string) map[string]int64 {
	out := map[string]int64{}
	lines := strings.Split(text, "\n")
	for _, prefix := range prefixes {
		for i := 0; i+1 < len(lines); i++ {
			h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
			if len(h) == 0 || len(v) == 0 || h[0] != prefix || v[0] != prefix {
				continue
			}
			for j := 1; j < len(h) && j < len(v); j++ {
				if n, err := strconv.ParseInt(v[j], 10, 64); err == nil {
					out[h[j]] = n
				}
			}
			break
		}
	}
	return out
}

// the columns of /proc/net/softnet_stat the report reads (0-based).
const (
	softnetDroppedCol896 = 1  // packets dropped for want of room in the CPU's backlog
	softnetSqueezeCol896 = 2  // softirq runs that ran out of budget
	softnetBacklogCol896 = 11 // the CPU's backlog length, input and process queues together, kernels from 5.10
	// (from 6.7, 13 is the input queue alone and 14 the process queue, the part
	// process_backlog has already taken: a SYN waiting for the softirq is not in it)
)

// softnetSum896 sums column col (0-based; the values are hex) of
// /proc/net/softnet_stat over the CPUs: 1 is the packets dropped for want of
// room in the CPU's backlog, 2 the softirq runs that ran out of budget.
func softnetSum896(text string, col int) int64 {
	var n int64
	for _, ln := range strings.Split(text, "\n") {
		if fl := strings.Fields(ln); len(fl) > col {
			if v, err := strconv.ParseInt(fl[col], 16, 64); err == nil {
				n += v
			}
		}
	}
	return n
}

// softnetBacklog896 sums the backlog column of /proc/net/softnet_stat (the
// length of the CPU's backlog queue, input and process queues together) over
// the CPUs: a SYN that sits unprocessed shows here.
func softnetBacklog896() int64 {
	b, err := os.ReadFile("/proc/net/softnet_stat")
	if err != nil {
		return 0
	}
	return softnetSum896(string(b), softnetBacklogCol896)
}

// fixtures of the /proc files, with values that tell the columns apart.
const (
	netstatFixture896 = "TcpExt: SyncookiesSent ListenOverflows ListenDrops TCPReqQFullDrop\n" +
		"TcpExt: 1 22 333 4444\n" +
		"IpExt: InNoRoutes InOctets\n" +
		"IpExt: 5 66\n" +
		"MPTcpExt: ListenDrops\nMPTcpExt: 9999\n"
	snmpFixture896 = "Ip: Forwarding\nIp: 1\n" +
		"Tcp: RtoAlgorithm AttemptFails EstabResets RetransSegs\n" +
		"Tcp: 1 7 8 9\n"
	// two CPUs, hex, as net/core/net-procfs.c prints them: processed, dropped,
	// time_squeeze, five zero columns, cpu_collision 0, received_rps,
	// flow_limit_count, the backlog length (11), the CPU index (12), and from
	// kernel 6.7 the input queue (13) and the process queue (14) the length is
	// the sum of. The first row is of that kernel (backlog 5 = 3 + 2), the
	// second of 5.10 to 6.6, 13 columns (backlog 4).
	softnetFixture896 = "0000aaaa 00000010 00000003 0 0 0 0 0 0 00000007 00000009 00000005 00000000 00000003 00000002\n" +
		"0000bbbb 00000020 00000005 0 0 0 0 0 0 00000008 0000000a 00000004 00000001\n"
)

// tcpRowFixture896 is a /proc/net/tcp row whose local port is 0x1F90 (8080),
// in the state st, with the retransmit count rt.
func tcpRowFixture896(st string, rt int) string {
	return fmt.Sprintf("   0: 0100007F:1F90 0100007F:C350 %s 00000000:00000001 00:00000000 %08X     0        0 12345 1 0000000000000000\n", st, rt)
}

// TestDialWatchReadsTheProcFiles1229 pins what the report says with fixed
// text, so that a column or a name read off by one, a state name swapped or a
// delta that is always 0 is a failure here and not a wrong report in a
// failed CI run.
func TestDialWatchReadsTheProcFiles1229(t *testing.T) {
	const hdr = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	for st, name := range map[string]string{"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1",
		"05": "FIN_WAIT2", "06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING"} {
		rows := tcpRowsOf896("tcp", hdr+tcpRowFixture896(st, 0), 8080)
		want := "tcp 0100007F:1F90>0100007F:C350 " + name + " q(tx:rx)=00000000:00000001"
		if len(rows) != 1 || !strings.HasPrefix(rows[0], want) {
			t.Errorf("state %s: rows = %q, want one starting %q", st, rows, want)
		}
	}
	if rows := tcpRowsOf896("tcp", hdr+tcpRowFixture896("02", 12), 8080); len(rows) != 1 || !strings.HasSuffix(rows[0], " retrnsmt=12") {
		t.Errorf("a retransmitted SYN_SENT row = %q, want retrnsmt=12", rows)
	}
	// The remote port counts too (the client's row), a port that is neither does not.
	if rows := tcpRowsOf896("tcp", hdr+tcpRowFixture896("01", 0), 0xC350); len(rows) != 1 {
		t.Errorf("rows of the remote port = %q, want the one row", rows)
	}
	if rows := tcpRowsOf896("tcp", hdr+tcpRowFixture896("01", 0), 8081); len(rows) != 0 {
		t.Errorf("rows of another port = %q, want none", rows)
	}
	if got := countState896([]string{"tcp a>b SYN_RECV q(tx:rx)=0:0", "tcp c>d LISTEN q(tx:rx)=0:0"}, "SYN_RECV"); got != 1 {
		t.Errorf("countState896 = %d, want 1", got)
	}

	ext := parseProcPairs896(netstatFixture896, "TcpExt:", "IpExt:")
	for k, want := range map[string]int64{"SyncookiesSent": 1, "ListenOverflows": 22, "ListenDrops": 333, "TCPReqQFullDrop": 4444, "InNoRoutes": 5, "InOctets": 66} {
		if ext[k] != want {
			t.Errorf("netstat %s = %d, want %d", k, ext[k], want)
		}
	}
	tcp := parseProcPairs896(snmpFixture896, "Tcp:")
	for k, want := range map[string]int64{"AttemptFails": 7, "EstabResets": 8, "RetransSegs": 9} {
		if tcp[k] != want {
			t.Errorf("snmp Tcp %s = %d, want %d", k, tcp[k], want)
		}
	}
	if got := softnetSum896(softnetFixture896, softnetDroppedCol896); got != 0x30 {
		t.Errorf("softnet dropped = %#x, want 0x30", got)
	}
	if got := softnetSum896(softnetFixture896, softnetSqueezeCol896); got != 8 {
		t.Errorf("softnet squeeze = %d, want 8", got)
	}
	if got := softnetSum896(softnetFixture896, softnetBacklogCol896); got != 9 {
		t.Errorf("softnet backlog length = %d, want 9 (5 + 4)", got)
	}

	before := map[string]int64{"ListenDrops": 300, "ListenOverflows": 22}
	got := strings.Join(deltas896([]string{"ListenDrops", "ListenOverflows", "Absent"}, before, ext), " ")
	if want := "ListenDrops+33 ListenOverflows+0"; got != want {
		t.Errorf("deltas = %q, want %q", got, want)
	}
}

// TestDialWatchGapSeesAStallThatSpansTheDeadline1229: the gap is measured
// with the clock and not with the value the ticker delivers (the time the tick
// was scheduled for, 10 ms after the last), and the stop counts the gap since
// the last tick: a stall that began before the dial's deadline ends with the
// deadline, and the next tick comes after the report.
func TestDialWatchGapSeesAStallThatSpansTheDeadline1229(t *testing.T) {
	var g gapTracker896
	t0 := time.Unix(1000, 0)
	g.last = t0
	g.tick(t0.Add(10 * time.Millisecond))
	g.tick(t0.Add(20 * time.Millisecond))
	g.tick(t0.Add(1520 * time.Millisecond))
	g.tick(t0.Add(1530 * time.Millisecond))
	if got := time.Duration(g.max.Load()); got != 1500*time.Millisecond {
		t.Errorf("largest gap = %v, want 1.5s", got)
	}

	// The watch's own handlers, driven with a clock that jumps and no ticker
	// or goroutine: both the tick and the stop take the clock's time.
	for _, onStop := range []bool{false, true} {
		now := t0
		w := &dialWatch896{now: func() time.Time { return now }}
		w.gaps.last = t0
		now = t0.Add(1500 * time.Millisecond)
		if onStop {
			w.onStop()
		} else {
			w.onTick()
		}
		if got := time.Duration(w.gaps.max.Load()); got != 1500*time.Millisecond {
			t.Errorf("stall seen by the %s handler: largest gap = %v, want 1.5s", map[bool]string{false: "tick", true: "stop"}[onStop], got)
		}
	}
}

// TestDialWatchSnapshotKeepsTheLastRowsFromHalfTheTimeout1229: a snapshot
// before half the timeout is not kept (the dial has only just begun and its
// first moments would stand in for the deadline's), later ones replace each
// other, and one without a row does not erase the last that had one.
func TestDialWatchSnapshotKeepsTheLastRowsFromHalfTheTimeout1229(t *testing.T) {
	t0 := time.Unix(1000, 0)
	now := t0
	rows := []string{"tcp a>b SYN_SENT q(tx:rx)=00000001:00000000"}
	var backlog int64 = 7
	w := &dialWatch896{now: func() time.Time { return now }, t0: t0, timeout: 2 * time.Second, port: 8080,
		rows: func(int) []string { return rows }, backlog: func() int64 { return backlog },
		stop: make(chan struct{}), finished: make(chan struct{})}
	close(w.finished) // no goroutine: report's end() has nothing to wait for

	now = t0.Add(time.Second - time.Millisecond)
	w.onSnap()
	if w.near != nil {
		t.Errorf("a snapshot was kept before half of the timeout: %q at %v", w.near, w.nearAt)
	}
	now = t0.Add(time.Second)
	w.onSnap()
	if len(w.near) != 1 || w.nearAt != time.Second || w.backlogN != 7 {
		t.Errorf("snapshot at half the timeout = %q at %v backlog %d, want the row at 1s with backlog 7", w.near, w.nearAt, w.backlogN)
	}
	rows, backlog, now = []string{"tcp c>d TIME_WAIT q(tx:rx)=0:0"}, 9, t0.Add(1500*time.Millisecond)
	w.onSnap()
	if len(w.near) != 1 || !strings.Contains(w.near[0], "TIME_WAIT") || w.nearAt != 1500*time.Millisecond || w.backlogN != 9 {
		t.Errorf("a later snapshot = %q at %v backlog %d, want it to replace the first", w.near, w.nearAt, w.backlogN)
	}
	rows, now = nil, t0.Add(1800*time.Millisecond)
	w.onSnap()
	if len(w.near) != 1 || w.nearAt != 1500*time.Millisecond {
		t.Errorf("a snapshot without a row replaced %q at %v", w.near, w.nearAt)
	}
	if got := w.report(); !strings.Contains(got, "near the deadline at 1.5s: [tcp c>d TIME_WAIT") {
		t.Errorf("report does not carry the kept snapshot: %s", got)
	}
}

// TestDialWatchDescribesTheSocketsOfAPort1229 runs the watch against live
// sockets and asserts only what holds in any environment: the report is made,
// bounded, names the port and the gap, and ends the watch (the fixture tests
// above carry what the fields say). What the sockets show depends on the
// host, so it is logged; a host that gives a dial nobody answers (a reserved
// address, whose SYN is not answered) is not one the test can rely on, and
// the SYN_SENT row of its snapshot is checked only if the dial was left
// unanswered.
func TestDialWatchDescribesTheSocketsOfAPort1229(t *testing.T) {
	check := func(t *testing.T, got string, port int) {
		t.Helper()
		if len(got) > 4000 {
			t.Errorf("report of %d bytes is not bounded", len(got))
		}
		for _, want := range []string{fmt.Sprintf("sockets on port %d:", port), "largest gap between 10 ms ticks"} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q: %s", want, got)
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener here: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	w := watchDial896(addr, dialTimeout896)
	c, err := net.DialTimeout("tcp", addr, dialTimeout896)
	if err != nil {
		_ = ln.Close()
		t.Skipf("no loopback dial here: %v", err)
	}
	got := w.report()
	t.Logf("report: %s", got)
	check(t, got, port)
	_ = c.Close()
	_ = ln.Close()

	// A dial nobody answers: a reserved address, on a port of our own choosing.
	unanswered := fmt.Sprintf("10.255.255.1:%d", port)
	w = watchDial896(unanswered, time.Second)
	t0 := time.Now()
	c, err = net.DialTimeout("tcp", unanswered, time.Second)
	if err == nil {
		_ = c.Close()
	}
	got = w.report()
	t.Logf("report: %s", got)
	check(t, got, port)
	w.end() // a second end after report is harmless
	if err == nil || time.Since(t0) < 900*time.Millisecond || !strings.Contains(got, "SYN_SENT") {
		t.Skipf("this host did not leave the dial to %s unanswered with a SYN_SENT row in the snapshot (err %v after %v)",
			unanswered, err, time.Since(t0).Round(time.Millisecond))
	}
}
