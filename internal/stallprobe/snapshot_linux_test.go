//go:build linux

package stallprobe

import (
	"fmt"
	"math"
	"net"
	"os"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The stall-time snapshot. When a client repetition has seen no byte for
// CELERIS_PROBE_SNAP_MS (2000 ms), the client captures the state of the
// process and of the host, waits CELERIS_PROBE_SNAP_GAP_MS (500 ms), captures
// it again, and then goes on waiting: the 5 s no-byte rule is not changed (the
// snapshots happen inside the wait, against an absolute deadline). Everything
// is read without privileges:
//
//   - /proc/self/task/<tid>/{stat,schedstat,wchan} of every thread: state, the
//     CPU it last ran on, user and system ticks, nanoseconds on a CPU, nanoseconds
//     runnable and waiting, the number of times it was scheduled in. The
//     deltas between the two captures say which side is dead: a thread that
//     was scheduled in 0 times in 500 ms did not run; one that was scheduled
//     in hundreds of times is alive and polling (an epoll loop wakes every 1-4
//     ms by its own timeout, so an epoll loop that is alive and a transfer
//     that is stuck points to an event or a send celeris did not act on).
//   - /proc/net/tcp and tcp6: the server-side socket's Send-Q and the
//     client-side socket's Recv-Q (matched by port pair), their timers, and
//     every LISTEN row on the server port with its accept queue. Bytes in
//     the client's Recv-Q mean the client did not read them. Bytes in the
//     server's Send-Q with an empty client Recv-Q and no timer armed mean the
//     sender holds data and nothing moves it.
//   - the client socket's TCP_INFO (time since the last data received, receive
//     space, retransmits).
//   - /proc/pressure/{cpu,io,memory}, per-CPU scaling_cur_freq and the cpuidle
//     usage and residency deltas, and the Go runtime's goroutine count and
//     stop-the-world pause histogram deltas since the leaf began.
//
// The result is one full dump (to the test log through logf) and a digest of
// a few lines that goes into the failure message itself, because the stress
// tally keeps the failure text of a failing leaf.

// ---- knobs -------------------------------------------------------------------

type probeCfg struct {
	SnapAfter time.Duration // first capture, after this long without a byte (0 disables)
	SnapGap   time.Duration // second capture, this long after the first
	Slow      time.Duration // a rep whose longest wait reaches this is counted as "stalled"
	// Fault injection for proving the snapshot path. Off by default: a handler
	// serving every InjectEvery-th /big request sleeps InjectMS first.
	InjectMS    int
	InjectEvery int
	Prof        bool // CPU profile of the gap between the two captures (CELERIS_PROBE_PROF=1; default OFF: SIGPROF to every thread can reshape a two-thread busy state)
	GDump       bool // timed goroutine dump after the second capture (CELERIS_PROBE_GDUMP=1, default off)
}

var pcfg = loadCfg()

func loadCfg() probeCfg {
	return probeCfg{
		SnapAfter:   time.Duration(envInt("CELERIS_PROBE_SNAP_MS", 2000)) * time.Millisecond,
		SnapGap:     time.Duration(envInt("CELERIS_PROBE_SNAP_GAP_MS", 500)) * time.Millisecond,
		Slow:        time.Duration(envInt("CELERIS_PROBE_SLOW_MS", 1000)) * time.Millisecond,
		InjectMS:    envInt("CELERIS_PROBE_INJECT_MS", 0),
		InjectEvery: max(envInt("CELERIS_PROBE_INJECT_EVERY", 5), 1),
		Prof:        envInt("CELERIS_PROBE_PROF", 0) == 1,
		GDump:       envInt("CELERIS_PROBE_GDUMP", 0) == 1,
	}
}

var injectSeq struct {
	mu sync.Mutex
	n  int
}

// maybeInject sleeps in a handler when the injection knob is on.
func maybeInject() {
	if pcfg.InjectMS <= 0 {
		return
	}
	injectSeq.mu.Lock()
	injectSeq.n++
	hit := injectSeq.n%pcfg.InjectEvery == 0
	injectSeq.mu.Unlock()
	if hit {
		time.Sleep(time.Duration(pcfg.InjectMS) * time.Millisecond)
	}
}

// ---- what the snapshot knows about the server -----------------------------------

var snapServer struct {
	mu    sync.Mutex
	loops []loopThread
	topo  *topo
}

func snapSetLoops(loops []loopThread) {
	snapServer.mu.Lock()
	snapServer.loops = append([]loopThread(nil), loops...)
	snapServer.mu.Unlock()
}

func snapLoops() []loopThread {
	snapServer.mu.Lock()
	defer snapServer.mu.Unlock()
	return append([]loopThread(nil), snapServer.loops...)
}

func snapTopo() topo {
	snapServer.mu.Lock()
	defer snapServer.mu.Unlock()
	if snapServer.topo == nil {
		t := loadTopo()
		snapServer.topo = &t
	}
	return *snapServer.topo
}

// ---- the captured data -----------------------------------------------------------

type snapTask struct {
	Tid      int
	Comm     string
	State    string
	CPU      int
	UTime    uint64 // clock ticks (1/100 s)
	STime    uint64
	RunNs    uint64
	WaitNs   uint64
	Slices   uint64
	HasSched bool
	Wchan    string
	Allowed  string
	// Added after run 37975655016 (19 of 19 stalls had a loop pinned on an A520
	// running 100% with 0-3 sched-ins): voluntary and involuntary context
	// switches (/proc/<tid>/status) and the syscall the thread is in
	// (/proc/<tid>/syscall: "running" for a thread on a CPU, the number and
	// arguments for a blocked one; for epoll_pwait the first argument is the
	// epoll fd, which says which loop a connection belongs to).
	Vol, Invol uint64
	HasCtx     bool
	Syscall    string
}

type snapSock struct {
	Role   string // client-side, server-side, listener
	Local  string
	Remote string
	State  string
	TxQ    uint64
	RxQ    uint64
	Timer  int
	When   uint64 // ticks (1/100 s) until the timer fires
	Retr   uint64
	RTO    uint64 // ticks
	Cwnd   uint64
}

type psiVal struct {
	Avg10 string
	Total uint64 // microseconds
}

type idleState struct {
	Name    string
	Usage   uint64
	TimeUs  uint64
	HasData bool
}

type snapshot struct {
	AtNs       int64
	WaitedNs   int64
	Bytes      int
	Tasks      []snapTask
	Socks      []snapSock
	ClientInfo string
	ClientLast int64 // TCP_INFO last_data_recv, ms, -1 unknown
	PSI        map[string]psiVal
	Freq       map[int]int64
	Idle       map[int][]idleState
	Goroutines int
	STW        string
	Owner      string // connOwner at this capture
	Extra      string // the CPU profile of the gap and the goroutine dump, on the second capture
}

func takeSnapshot(c clk, conn net.Conn, cport, sport int, bytes int, waited time.Duration) *snapshot {
	s := &snapshot{AtNs: c.now(), WaitedNs: int64(waited), Bytes: bytes, ClientLast: -1}
	s.Tasks = readTasks()
	s.Socks = readSocks(cport, sport)
	s.ClientInfo, s.ClientLast = clientTCPInfo(conn)
	s.PSI = readPSI()
	s.Freq, s.Idle = readCPUState()
	s.Goroutines = runtime.NumGoroutine()
	s.STW = stwSinceLeaf()
	return s
}

func readTasks() []snapTask {
	var out []snapTask
	for _, tid := range taskIDs() {
		base := fmt.Sprintf("/proc/self/task/%d/", tid)
		st := readTrim(base + "stat")
		i, j := strings.IndexByte(st, '('), strings.LastIndexByte(st, ')')
		if i < 0 || j < i || j+2 > len(st) {
			continue
		}
		f := strings.Fields(st[j+2:])
		if len(f) < 37 {
			continue
		}
		t := snapTask{Tid: tid, Comm: st[i+1 : j], State: f[0], CPU: -1, Wchan: readTrim(base + "wchan"), Allowed: threadAllowed(tid)}
		t.UTime, _ = strconv.ParseUint(f[11], 10, 64)
		t.STime, _ = strconv.ParseUint(f[12], 10, 64)
		if n, err := strconv.Atoi(f[36]); err == nil {
			t.CPU = n
		}
		if ss := strings.Fields(readTrim(base + "schedstat")); len(ss) >= 3 {
			t.RunNs, _ = strconv.ParseUint(ss[0], 10, 64)
			t.WaitNs, _ = strconv.ParseUint(ss[1], 10, 64)
			t.Slices, _ = strconv.ParseUint(ss[2], 10, 64)
			t.HasSched = true
		}
		if stt := readTrim(base + "status"); stt != "" {
			for _, l := range strings.Split(stt, "\n") {
				if k, v, ok := strings.Cut(l, ":"); ok {
					switch k {
					case "voluntary_ctxt_switches":
						t.Vol, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
						t.HasCtx = true
					case "nonvoluntary_ctxt_switches":
						t.Invol, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
					}
				}
			}
		}
		t.Syscall = readTrim(base + "syscall")
		out = append(out, t)
	}
	return out
}

var tcpStates = map[string]string{
	"01": "ESTAB", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2",
	"06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING",
}

func hexPort(addr string) int {
	if k := strings.LastIndexByte(addr, ':'); k >= 0 {
		if n, err := strconv.ParseUint(addr[k+1:], 16, 32); err == nil {
			return int(n)
		}
	}
	return -1
}

func hexAddr(addr string) string {
	k := strings.LastIndexByte(addr, ':')
	if k < 0 {
		return addr
	}
	h, p := addr[:k], hexPort(addr)
	if len(h) == 8 { // IPv4, little endian words
		b, err := strconv.ParseUint(h, 16, 32)
		if err == nil {
			return fmt.Sprintf("%d.%d.%d.%d:%d", b&0xff, b>>8&0xff, b>>16&0xff, b>>24&0xff, p)
		}
	}
	return fmt.Sprintf("[%s]:%d", h, p)
}

func readSocks(cport, sport int) []snapSock {
	var out []snapSock
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 17 {
				continue
			}
			lp, rp := hexPort(f[1]), hexPort(f[2])
			st := f[3]
			role := ""
			switch {
			case st == "0A" && lp == sport:
				role = "listener"
			case lp == sport && rp == cport && st != "0A":
				role = "server-side"
			case lp == cport && rp == sport:
				role = "client-side"
			default:
				continue
			}
			s := snapSock{Role: role, Local: hexAddr(f[1]), Remote: hexAddr(f[2]), State: tcpStates[st]}
			if s.State == "" {
				s.State = st
			}
			if q := strings.Split(f[4], ":"); len(q) == 2 {
				s.TxQ, _ = strconv.ParseUint(q[0], 16, 64)
				s.RxQ, _ = strconv.ParseUint(q[1], 16, 64)
			}
			if q := strings.Split(f[5], ":"); len(q) == 2 {
				n, _ := strconv.ParseUint(q[0], 16, 32)
				s.Timer = int(n)
				s.When, _ = strconv.ParseUint(q[1], 16, 64)
			}
			s.Retr, _ = strconv.ParseUint(f[6], 16, 64)
			s.RTO, _ = strconv.ParseUint(f[12], 10, 64)
			s.Cwnd, _ = strconv.ParseUint(f[15], 10, 64)
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out
}

func clientTCPInfo(conn net.Conn) (string, int64) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return "no TCP_INFO (not a TCPConn)", -1
	}
	sc, err := tc.SyscallConn()
	if err != nil {
		return "no TCP_INFO: " + err.Error(), -1
	}
	var info *unix.TCPInfo
	var ierr error
	if err := sc.Control(func(fd uintptr) { info, ierr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); err != nil || ierr != nil || info == nil {
		return fmt.Sprintf("no TCP_INFO: %v %v", err, ierr), -1
	}
	return fmt.Sprintf("last_data_recv %d ms, last_ack_sent %d ms, rcv_space %d, rcv_ssthresh %d, rcv_mss %d, rtt %d us, retrans %d (total %d), unacked %d, ca_state %d",
		info.Last_data_recv, info.Last_ack_sent, info.Rcv_space, info.Rcv_ssthresh, info.Rcv_mss, info.Rtt, info.Retrans, info.Total_retrans, info.Unacked, info.Ca_state), int64(info.Last_data_recv)
}

func readPSI() map[string]psiVal {
	out := map[string]psiVal{}
	for _, res := range []string{"cpu", "io", "memory"} {
		for _, l := range strings.Split(readTrim("/proc/pressure/"+res), "\n") {
			f := strings.Fields(l)
			if len(f) < 5 || (f[0] != "some" && f[0] != "full") {
				continue
			}
			v := psiVal{}
			for _, kv := range f[1:] {
				k, val, _ := strings.Cut(kv, "=")
				switch k {
				case "avg10":
					v.Avg10 = val
				case "total":
					v.Total, _ = strconv.ParseUint(val, 10, 64)
				}
			}
			out[res+" "+f[0]] = v
		}
	}
	return out
}

func readCPUState() (map[int]int64, map[int][]idleState) {
	freq := map[int]int64{}
	idle := map[int][]idleState{}
	online, err := parseCPUList(readTrim("/sys/devices/system/cpu/online"))
	if err != nil || len(online) == 0 {
		online = snapTopo().Allowed
	}
	for _, c := range online {
		d := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/", c)
		freq[c] = int64(readInt(d+"cpufreq/scaling_cur_freq", -1))
		for k := 0; k < 16; k++ {
			sd := fmt.Sprintf("%scpuidle/state%d/", d, k)
			name := readTrim(sd + "name")
			if name == "" {
				break
			}
			u, e1 := strconv.ParseUint(readTrim(sd+"usage"), 10, 64)
			tm, e2 := strconv.ParseUint(readTrim(sd+"time"), 10, 64)
			idle[c] = append(idle[c], idleState{Name: name, Usage: u, TimeUs: tm, HasData: e1 == nil && e2 == nil})
		}
	}
	return freq, idle
}

// ---- runtime stop-the-world pauses (H5, runtime branch) --------------------------

var stwNames = []string{
	"/sched/pauses/stopping/gc:seconds",
	"/sched/pauses/stopping/other:seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/pauses/total/other:seconds",
}

type stwSample struct {
	names  []string
	hists  []*metrics.Float64Histogram
	cycles uint64
}

func readSTW() stwSample {
	have := map[string]bool{}
	for _, d := range metrics.All() {
		have[d.Name] = true
	}
	var s stwSample
	var ms []metrics.Sample
	for _, n := range stwNames {
		if have[n] {
			ms = append(ms, metrics.Sample{Name: n})
			s.names = append(s.names, n)
		}
	}
	if have["/gc/cycles/total:gc-cycles"] {
		ms = append(ms, metrics.Sample{Name: "/gc/cycles/total:gc-cycles"})
	}
	metrics.Read(ms)
	for _, m := range ms {
		switch m.Value.Kind() {
		case metrics.KindFloat64Histogram:
			h := m.Value.Float64Histogram()
			s.hists = append(s.hists, &metrics.Float64Histogram{Counts: append([]uint64(nil), h.Counts...), Buckets: h.Buckets})
		case metrics.KindUint64:
			s.cycles = m.Value.Uint64()
		}
	}
	return s
}

// describe reports, per pause metric, the number of pauses between two
// samples and the upper bound of the longest populated bucket.
func (b stwSample) since(a stwSample) string {
	var parts []string
	for i, n := range b.names {
		if i >= len(a.hists) || i >= len(b.hists) {
			break
		}
		ha, hb := a.hists[i], b.hists[i]
		var cnt uint64
		top := -1
		for k := range hb.Counts {
			var was uint64
			if k < len(ha.Counts) {
				was = ha.Counts[k]
			}
			if hb.Counts[k] > was {
				cnt += hb.Counts[k] - was
				top = k
			}
		}
		short := strings.TrimSuffix(strings.TrimPrefix(n, "/sched/pauses/"), ":seconds")
		if top < 0 {
			parts = append(parts, short+" none")
			continue
		}
		up := hb.Buckets[top+1]
		if math.IsInf(up, 1) {
			parts = append(parts, fmt.Sprintf("%s %d pauses, longest above %s ms", short, cnt, fmtMs(hb.Buckets[top]*1000)))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d pauses, longest <= %s ms", short, cnt, fmtMs(up*1000)))
		}
	}
	return fmt.Sprintf("GC cycles %d; ", b.cycles-a.cycles) + strings.Join(parts, "; ")
}

func fmtMs(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.3f", v)
}

var leafSTW struct {
	mu   sync.Mutex
	base stwSample
	set  bool
}

func stwLeafBegin() {
	leafSTW.mu.Lock()
	leafSTW.base, leafSTW.set = readSTW(), true
	leafSTW.mu.Unlock()
}

func stwLeafEnd() string {
	cur := readSTW()
	leafSTW.mu.Lock()
	defer leafSTW.mu.Unlock()
	if !leafSTW.set {
		return "n/a"
	}
	return cur.since(leafSTW.base)
}

func stwSinceLeaf() string { return stwLeafEnd() }

// ---- describing a pair of snapshots -------------------------------------------------

type taskDelta struct {
	snapTask
	From           snapTask
	dSlices, dRun  int64
	dWait, dTicks  int64
	dUser, dSys    int64
	dVol, dInvol   int64
	role           string
	isLoop, moved  bool
	prevCPU, wantC int
}

// describeSnapshots returns the full dump and the digest for one stall. b may
// be nil (the stall ended before the second capture).
func describeSnapshots(a, b *snapshot, loops []loopThread, outcome string) (full, digest string) {
	tp := snapTopo()
	cls := func(cpu int) string {
		if cpu < 0 {
			return "?"
		}
		if ci, ok := tp.CPU[cpu]; ok {
			return fmt.Sprintf("cpu%d/%s", cpu, ci.class())
		}
		return fmt.Sprintf("cpu%d", cpu)
	}
	last := a
	if b != nil {
		last = b
	}
	span := time.Duration(0)
	if b != nil {
		span = time.Duration(b.AtNs - a.AtNs)
	}
	prev := map[int]snapTask{}
	for _, t := range a.Tasks {
		prev[t.Tid] = t
	}
	loopOf := map[int]loopThread{}
	for i, l := range loops {
		_ = i
		loopOf[l.Tid] = l
	}
	var ds []taskDelta
	for _, t := range last.Tasks {
		d := taskDelta{snapTask: t, role: "go"}
		if l, ok := loopOf[t.Tid]; ok {
			d.isLoop, d.role = true, fmt.Sprintf("LOOP(pin %d)", l.From)
		} else if strings.HasPrefix(t.Comm, "iou-") {
			d.role = "io-wq"
		}
		if p, ok := prev[t.Tid]; ok && b != nil {
			d.From = p
			d.dSlices, d.dRun, d.dWait = int64(t.Slices)-int64(p.Slices), int64(t.RunNs)-int64(p.RunNs), int64(t.WaitNs)-int64(p.WaitNs)
			d.dTicks = int64(t.UTime+t.STime) - int64(p.UTime+p.STime)
			d.dUser, d.dSys = int64(t.UTime)-int64(p.UTime), int64(t.STime)-int64(p.STime)
			d.dVol, d.dInvol = int64(t.Vol)-int64(p.Vol), int64(t.Invol)-int64(p.Invol)
		}
		ds = append(ds, d)
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].isLoop != ds[j].isLoop {
			return ds[i].isLoop
		}
		return ds[i].Tid < ds[j].Tid
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "STALL SNAPSHOT: first capture %.0f ms and second %s after the last byte (%d bytes received); outcome: %s\n", float64(a.WaitedNs)/1e6, orNA(b, func() string { return fmt.Sprintf("%.0f ms", float64(b.WaitedNs)/1e6) }), a.Bytes, outcome)
	fmt.Fprintf(&sb, "threads (state and cpu as of the %s capture; deltas over %s):\n", map[bool]string{true: "second", false: "first"}[b != nil], span.Round(time.Millisecond))
	fmt.Fprintf(&sb, "  %-8s %-14s %-16s %-5s %-8s %-9s %-9s %-9s %-7s %s\n", "tid", "role", "comm", "state", "cpu", "d.sched", "d.run_ms", "d.wait_ms", "d.ticks", "wchan  allowed")
	for _, d := range ds {
		fmt.Fprintf(&sb, "  %-8d %-14s %-16s %-5s %-8s %-9d %-9.1f %-9.1f %-7d %s  %s  utime=%d stime=%d vol=%d invol=%d sys=%s\n", d.Tid, d.role, d.Comm, d.State, cls(d.CPU), d.dSlices, float64(d.dRun)/1e6, float64(d.dWait)/1e6, d.dTicks, orDash(d.Wchan), d.Allowed, d.dUser, d.dSys, d.dVol, d.dInvol, syscallWord(d.Syscall))
	}

	loopsAlive, loopsFrozen, loopsSpin := 0, 0, 0
	var loopBits []string
	for _, d := range ds {
		if !d.isLoop {
			continue
		}
		alive := d.dSlices >= 3 || d.dRun >= 2_000_000
		if b != nil && span > 0 && d.State == "R" && float64(d.dRun) >= 0.8*float64(span) && d.dSlices <= 3 {
			// On a CPU for the whole window and never scheduled out: it is
			// not "frozen" and it is not waking on a timeout either. Run
			// 37975655016 had exactly one of these in each of 19 stalls, always
			// a loop on a Cortex-A520, and the old rule counted it as frozen.
			loopsSpin++
			alive = false
		} else if alive {
			loopsAlive++
		} else if b != nil {
			loopsFrozen++
		}
		loopBits = append(loopBits, fmt.Sprintf("%d@%s %s +%dsched/%.0fms", d.Tid, cls(d.CPU), d.State, d.dSlices, float64(d.dRun)/1e6))
	}
	anyRan := 0
	for _, d := range ds {
		if d.dSlices >= 3 {
			anyRan++
		}
	}

	var srvQ, cliQ, lisQ int64 = -1, -1, -1
	var srvTimer, srvWhen = -1, uint64(0)
	fmt.Fprintf(&sb, "sockets (as of the %s capture):\n", map[bool]string{true: "second", false: "first"}[b != nil])
	listeners := 0
	for _, s := range last.Socks {
		fmt.Fprintf(&sb, "  %-11s %s -> %s %s Send-Q %d Recv-Q %d timer %d (%s, due in %d ticks of 10 ms) retransmit-count %d rto %d ticks cwnd %d\n", s.Role, s.Local, s.Remote, s.State, s.TxQ, s.RxQ, s.Timer, timerName(s.Timer), s.When, s.Retr, s.RTO, s.Cwnd)
		switch s.Role {
		case "server-side":
			srvQ, srvTimer, srvWhen = int64(s.TxQ), s.Timer, s.When
		case "client-side":
			cliQ = int64(s.RxQ)
		case "listener":
			listeners++
			lisQ = max(lisQ, int64(s.RxQ))
		}
	}
	if len(last.Socks) == 0 {
		fmt.Fprintf(&sb, "  none found in /proc/net/tcp{,6}\n")
	}
	fmt.Fprintf(&sb, "  client TCP_INFO: %s\n", last.ClientInfo)

	if b != nil {
		fmt.Fprintf(&sb, "pressure (stall totals gained over the %s; avg10 now):\n", span.Round(time.Millisecond))
	} else {
		fmt.Fprintf(&sb, "pressure (avg10 only, one capture):\n")
	}
	var psiKeys []string
	for k := range last.PSI {
		psiKeys = append(psiKeys, k)
	}
	sort.Strings(psiKeys)
	psiBits := []string{}
	for _, k := range psiKeys {
		v := last.PSI[k]
		d := ""
		if b != nil {
			d = fmt.Sprintf(" +%d us", int64(v.Total)-int64(a.PSI[k].Total))
		}
		fmt.Fprintf(&sb, "  %-12s avg10 %s%s\n", k, v.Avg10, d)
		if strings.HasSuffix(k, "some") && b != nil {
			psiBits = append(psiBits, fmt.Sprintf("%s +%dus", strings.TrimSuffix(k, " some"), int64(v.Total)-int64(a.PSI[k].Total)))
		}
	}
	if len(psiKeys) == 0 {
		fmt.Fprintf(&sb, "  /proc/pressure not readable\n")
		psiBits = append(psiBits, "PSI n/a")
	}

	var cpus []int
	for c := range last.Freq {
		cpus = append(cpus, c)
	}
	sort.Ints(cpus)
	fmt.Fprintf(&sb, "per-CPU frequency (MHz, first -> second capture) and cpuidle entries and residency gained:\n")
	freqBits := []string{}
	for _, c := range cpus {
		f0 := a.Freq[c]
		fs := fmt.Sprintf("%s", mhz(f0))
		if b != nil {
			fs = fmt.Sprintf("%s->%s", mhz(f0), mhz(b.Freq[c]))
		}
		var ibits []string
		for k, st := range last.Idle[c] {
			if b != nil && k < len(a.Idle[c]) && st.HasData {
				du := int64(st.Usage) - int64(a.Idle[c][k].Usage)
				dt := int64(st.TimeUs) - int64(a.Idle[c][k].TimeUs)
				if du != 0 || dt != 0 {
					ibits = append(ibits, fmt.Sprintf("%s +%d/%dus", st.Name, du, dt))
				}
			}
		}
		idleS := "-"
		if len(last.Idle[c]) == 0 {
			idleS = "no cpuidle data"
		} else if len(ibits) > 0 {
			idleS = strings.Join(ibits, " ")
		} else if b != nil {
			idleS = "no idle entries"
		}
		fmt.Fprintf(&sb, "  %-9s %-11s idle: %s\n", cls(c), fs, idleS)
		freqBits = append(freqBits, fmt.Sprintf("%d:%s", c, fs))
	}
	fmt.Fprintf(&sb, "runtime: %d goroutines; since the leaf began: %s\n", last.Goroutines, last.STW)

	// ---- the digest ----
	var db strings.Builder
	fmt.Fprintf(&db, "stall snapshot (no byte for %.0f ms and %s, %d bytes in; %s): ", float64(a.WaitedNs)/1e6, orNA(b, func() string { return fmt.Sprintf("%.0f ms", float64(b.WaitedNs)/1e6) }), a.Bytes, outcome)
	if len(loops) > 0 {
		fmt.Fprintf(&db, "\n  loop threads: %d of %d ran (alive), %d did not (frozen) over %s: %s", loopsAlive, len(loops), loopsFrozen, span.Round(time.Millisecond), strings.Join(loopBits, "; "))
	} else {
		fmt.Fprintf(&db, "\n  no loop threads known (std or unmatched); %d threads were scheduled 3+ times over %s", anyRan, span.Round(time.Millisecond))
	}
	fmt.Fprintf(&db, "\n  sockets: server-side Send-Q %s (timer %s, due %d), client-side Recv-Q %s, listeners %d with accept queue %s; client %s",
		qs(srvQ), timerName(srvTimer), srvWhen, qs(cliQ), listeners, qs(lisQ), strings.SplitN(last.ClientInfo, ",", 2)[0])
	fmt.Fprintf(&db, "\n  host: PSI %s; MHz %s", strings.Join(psiBits, ", "), strings.Join(freqBits, " "))
	fmt.Fprintf(&db, "\n  runtime: %s", last.STW)
	fmt.Fprintf(&db, "\n  reading: %s", readingHint(a.Bytes, srvQ, cliQ, lisQ, loopsAlive, loopsFrozen, loopsSpin, len(loops), anyRan, b != nil))
	return sb.String(), db.String()
}

func orNA(b *snapshot, f func() string) string {
	if b == nil {
		return "(second capture not reached)"
	}
	return f()
}

func qs(v int64) string {
	if v < 0 {
		return "not found"
	}
	return strconv.FormatInt(v, 10)
}

func timerName(t int) string {
	switch t {
	case 0:
		return "none"
	case 1:
		return "retransmit"
	case 2:
		return "keepalive"
	case 3:
		return "time-wait"
	case 4:
		return "zero-window probe"
	case 5:
		return "loss probe"
	case -1:
		return "n/a"
	}
	return "?"
}

// readingHint is a mechanical reading of the numbers, not a verdict: what each
// pattern is consistent with.
func readingHint(bytes int, srvQ, cliQ, lisQ int64, alive, frozen, spin, nloops, anyRan int, haveB bool) string {
	var h []string
	switch {
	case cliQ > 0:
		h = append(h, "client Recv-Q > 0: bytes sit in the client's socket, unread; the client side (its goroutine or CPU) is what is not running")
	case lisQ > 0 && bytes == 0:
		h = append(h, "the listener accept queue is not empty while no byte came back: a connection may never have been accepted")
	case srvQ > 0 && cliQ == 0:
		h = append(h, "server Send-Q > 0 and client Recv-Q = 0: the sender holds bytes that are not moving (a zero-window probe or retransmit timer says TCP is trying; no timer says nobody is pushing)")
	case srvQ == 0 && cliQ == 0:
		h = append(h, "nothing queued on either socket: the server has not written the next bytes")
	case srvQ < 0 || cliQ < 0:
		h = append(h, "a socket row was not found")
	}
	if haveB && nloops > 0 {
		if spin > 0 {
			h = append(h, fmt.Sprintf("%d loop thread(s) ON A CPU THE WHOLE WINDOW without being scheduled out (spinning, not frozen, not waking on its timeout): see the thread table for its CPU, utime/stime and syscall", spin))
		}
		switch {
		case spin > 0 && alive+spin == nloops:
			h = append(h, fmt.Sprintf("the other %d loop threads kept waking", alive))
		case frozen == nloops:
			h = append(h, "every loop thread was scheduled < 3 times in the window: the loops did not run (a thread or CPU stall; for epoll this includes its own 1-4 ms timeout, io_uring may legitimately sleep in the ring wait)")
		case alive == nloops:
			h = append(h, "every loop thread kept running: the loops are alive, so a missed wakeup cannot explain the stall")
		default:
			h = append(h, fmt.Sprintf("%d loop threads ran and %d did not", alive, frozen))
		}
	}
	return strings.Join(h, "; ")
}

// syscallWord shortens /proc/<tid>/syscall to "running", "-1" (blocked in the
// kernel, not in a syscall) or "<nr>(<first argument>)"; for epoll_pwait (22 on
// arm64, 232 on amd64) the first argument is the epoll fd.
func syscallWord(raw string) string {
	f := strings.Fields(raw)
	switch {
	case len(f) == 0:
		return "-"
	case f[0] == "running" || f[0] == "-1":
		return f[0]
	case len(f) >= 2:
		return f[0] + "(" + f[1] + ")"
	}
	return f[0]
}
