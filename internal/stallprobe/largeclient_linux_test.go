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
	"testing"
	"time"
)

// The large-response client of P2 and P3: the check celeris's own
// large_response_linux_test.go makes (checkH1Response761), kept to the same
// rule. One request on a new keep-alive connection; the response head and then
// every body byte must arrive, and a client that gets no byte for the idle cap
// (5 s) calls the response lost; the connection must then answer /ping.
//
// On top of the verdict, every repetition records the longest single wait for
// bytes (so a stall of 2 s, under the cap, is still seen), the byte offset the
// longest wait began at, the time to the response head, and, when a wait for
// bytes reaches CELERIS_PROBE_SNAP_MS, the stall-time snapshot of
// snapshot_linux_test.go. There is no per-CPU attribution of a repetition:
// SO_INCOMING_CPU on the client socket records only where the last packet was
// processed, which on loopback is the client's own CPU for the handshake and
// the sender's for the data, so it identifies neither the serving loop nor the
// CPU a stalled response should have come from.

type snapRec struct {
	WaitedMs  float64 // how long the wait had lasted at the first capture
	Bytes     int
	Outcome   string
	Full      string
	Digest    string
	Recovered bool
}

type repResult struct {
	Rep         int
	OK          bool
	Err         string
	StartNs     int64
	EndNs       int64
	HeadNs      int64
	MaxIdleNs   int64
	MaxIdleAt   int // bytes received when the longest wait began
	Snaps       []snapRec
	ElapsedSnap int64 // ns spent taking snapshots (inside the waits)
}

type leafResult struct {
	Name    string
	MiB     int
	StartNs int64
	EndNs   int64
	Reps    []repResult
	Stopped bool   // stopped early: the stall budget was spent
	STW     string // runtime pause histogram deltas over the leaf
	// The process watchdog's oversleeps inside the leaf (see applyWD).
	WDMax       int64
	WD50, WD500 int
}

// applyWD sets the watchdog summary for the leaf's window.
func (l *leafResult) applyWD(w wdResult) {
	l.WDMax, l.WD50, l.WD500 = wdWithin(w, l.StartNs, l.EndNs)
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>13)
	}
	return b
}

// idleConn enforces the no-byte rule and takes the stall-time snapshots. The
// rule is an ABSOLUTE deadline of t0+idle for each Read, and the earlier
// deadlines the snapshots wait for are internal: a timeout before t0+idle is
// never returned to the caller (so bufio and http.ReadResponse never see
// it), and the time a snapshot takes counts against the 5 s.
type idleConn struct {
	net.Conn
	c      clk
	idle   time.Duration
	n      int
	max    int64
	maxAt  int
	cport  int
	sport  int
	snaps  []snapRec
	snapNs int64
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

const maxSnapsPerRep = 3

func (ic *idleConn) Read(p []byte) (int, error) {
	t0 := ic.c.now()
	final := t0 + int64(ic.idle)
	var a, b *snapshot
	var prof profCapture
	snapping := pcfg.SnapAfter > 0 && pcfg.SnapAfter+50*time.Millisecond < ic.idle && len(ic.snaps) < maxSnapsPerRep
	for {
		target := final
		if snapping {
			switch {
			case a == nil:
				target = t0 + int64(pcfg.SnapAfter)
			case b == nil:
				if t := a.AtNs + int64(pcfg.SnapGap); t < final-int64(20*time.Millisecond) {
					target = t
				}
			}
		}
		_ = ic.SetReadDeadline(time.Now().Add(time.Duration(target - ic.c.now())))
		n, err := ic.Conn.Read(p)
		now := ic.c.now()
		if n == 0 && err != nil && isTimeout(err) && target < final {
			// An internal timeout: capture and go on waiting.
			s0 := ic.c.now()
			var profText string
			if a != nil {
				profText = prof.stop() // the profile covers the gap between the two captures
			}
			snap := takeSnapshot(ic.c, ic.Conn, ic.cport, ic.sport, ic.n, time.Duration(now-t0))
			snap.Owner = connOwner(ic.cport, ic.sport, snap.Tasks, snapLoops())
			if a != nil {
				snap.Extra = profText
				if g := goroutineDump(); g != "" {
					snap.Extra += "\n" + g
				}
			}
			ic.snapNs += ic.c.now() - s0
			if a == nil {
				a = snap
				prof.start()
			} else {
				b = snap
			}
			continue
		}
		if w := now - t0; w > ic.max {
			ic.max, ic.maxAt = w, ic.n
		}
		ic.n += n
		if a != nil {
			outcome := "the wait went on to the end"
			rec := snapRec{WaitedMs: float64(a.WaitedNs) / 1e6, Bytes: a.Bytes}
			if n > 0 {
				outcome = fmt.Sprintf("RECOVERED: bytes arrived after %.0f ms without one", float64(now-t0)/1e6)
				rec.Recovered = true
			} else if !isTimeout(err) {
				outcome = "the wait ended with " + describeEnd(err, ic.idle)
			} else {
				outcome = fmt.Sprintf("NO BYTE for %v", ic.idle)
			}
			rec.Outcome = outcome
			rec.Full, rec.Digest = describeSnapshots(a, b, snapLoops(), outcome)
			if prof.on { // the wait ended before the second capture
				if pt := prof.stop(); pt != "" {
					rec.Full += "\n" + pt
				}
			}
			if a.Owner != "" {
				rec.Full += "\nfirst capture: " + a.Owner
			}
			if b != nil {
				if b.Owner != "" {
					rec.Full += "\nsecond capture: " + b.Owner
				}
				if b.Extra != "" {
					rec.Full += "\n" + b.Extra
				}
			}
			ic.snaps = append(ic.snaps, rec)
		}
		return n, err
	}
}

func describeEnd(err error, idle time.Duration) string {
	switch {
	case err == nil:
		return "no error"
	case isTimeout(err):
		return fmt.Sprintf("no byte for %v, connection still open", idle)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	}
	return err.Error()
}

func portOf(a net.Addr) int {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.Port
	}
	return -1
}

// fetchLarge does one repetition. buf must hold len(want) bytes.
func fetchLarge(c clk, addr, target string, want, buf []byte, idle time.Duration, rep int) (r repResult) {
	r = repResult{Rep: rep, StartNs: c.now()}
	var ic *idleConn
	defer func() {
		r.EndNs = c.now()
		if ic != nil {
			r.MaxIdleNs, r.MaxIdleAt, r.Snaps, r.ElapsedSnap = ic.max, ic.maxAt, ic.snaps, ic.snapNs
		}
	}()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		r.Err = "dial: " + err.Error()
		return r
	}
	defer func() { _ = raw.Close() }()
	ic = &idleConn{Conn: raw, c: c, idle: idle, cport: portOf(raw.LocalAddr()), sport: portOf(raw.RemoteAddr())}
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

// leafRule says when a leaf stops. A leaf runs reps repetitions. It stops
// early only when the stall budget is spent: maxStalls failed repetitions AND
// at least minReps repetitions run (so every leaf of a comparison has enough
// transfers to give a rate before it can stop), or twice maxStalls failed
// repetitions whatever the count (a leaf that fails every time costs 5 s a
// repetition).
type leafRule struct {
	Reps, MinReps, MaxStalls int
}

func (r leafRule) stop(done, failed int) bool {
	if failed >= 2*r.MaxStalls {
		return true
	}
	return failed >= r.MaxStalls && done >= r.MinReps
}

// runLeaf makes the repetitions of one request under the leaf rule.
func runLeaf(c clk, name string, mib int, addr, target string, want, buf []byte, rule leafRule, idle time.Duration, hook func(done int)) leafResult {
	l := leafResult{Name: name, MiB: mib, StartNs: c.now()}
	stwLeafBegin()
	failed := 0
	for i := 1; i <= rule.Reps; i++ {
		r := fetchLarge(c, addr, target, want, buf, idle, i)
		l.Reps = append(l.Reps, r)
		if !r.OK {
			failed++
		}
		if hook != nil {
			hook(i)
		}
		if rule.stop(i, failed) {
			l.Stopped = i < rule.Reps
			break
		}
	}
	l.STW = stwLeafEnd()
	l.EndNs = c.now()
	return l
}

func ruleFromEnv(rounds int) leafRule {
	rounds = max(rounds, 1)
	per := func(v int) int { return (v + rounds - 1) / rounds }
	return leafRule{
		Reps:      per(envInt("CELERIS_PROBE_REPS", 100)),
		MinReps:   per(envInt("CELERIS_PROBE_MINREPS", 40)),
		MaxStalls: max(per(envInt("CELERIS_PROBE_MAXSTALLS", 6)), 1),
	}
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func minMax(v []int64) (lo, hi int64) {
	if len(v) == 0 {
		return 0, 0
	}
	lo, hi = 1<<62, 0
	for _, x := range v {
		lo, hi = min(lo, x), max(hi, x)
	}
	return
}

// leafStats counts a leaf's transfers. A "stalled" repetition is one that
// failed or whose longest wait for bytes reached CELERIS_PROBE_SLOW_MS.
type leafStats struct {
	Transfers, Failed, Stalled, Snapped int
}

func (l leafResult) stats() leafStats {
	var s leafStats
	for _, r := range l.Reps {
		s.Transfers++
		if !r.OK {
			s.Failed++
		}
		if !r.OK || r.MaxIdleNs >= int64(pcfg.Slow) {
			s.Stalled++
		}
		if len(r.Snaps) > 0 {
			s.Snapped++
		}
	}
	return s
}

// reportLeaf logs one leaf's repetitions and fails t when any failed. The
// failure message of a repetition carries the digest of its snapshot, because
// the stress tally keeps the failure text of a failing leaf; the full dump
// goes to the log.
func reportLeaf(t *testing.T, l leafResult) {
	t.Helper()
	var tot, head, idle []int64
	st := l.stats()
	for _, r := range l.Reps {
		if r.OK {
			tot = append(tot, r.EndNs-r.StartNs)
		}
		head = append(head, r.HeadNs)
		idle = append(idle, r.MaxIdleNs)
	}
	tlo, thi := minMax(tot)
	_, hhi := minMax(head)
	_, ihi := minMax(idle)
	wmax, w50, w500 := l.WDMax, l.WD50, l.WD500
	logf(t, "reps run %d, ok %d, failed %d%s; stalled (failed or a wait >= %v) %d; reps with a stall-time snapshot %d\nok-rep total ms min/med/max %s/%s/%s; response head ms med/max %s/%s; longest wait for bytes ms med/max %s/%s\nprocess watchdog (1 ms sleeps) during the leaf: max oversleep %s ms, >=50 ms: %d, >=500 ms: %d\nruntime pauses during the leaf: %s",
		st.Transfers, st.Transfers-st.Failed, st.Failed, map[bool]string{true: " (stopped early: stall budget spent)", false: ""}[l.Stopped],
		pcfg.Slow, st.Stalled, st.Snapped,
		minOrDash(tot, tlo), ms(median(tot)), ms(thi), ms(median(head)), ms(hhi), ms(median(idle)), ms(ihi), ms(wmax), w50, w500, l.STW)
	for _, r := range l.Reps {
		for _, s := range r.Snaps {
			if r.OK {
				logf(t, "rep %d STALLED BUT PASSED (longest wait %s ms, began at byte %d)\n%s\n%s", r.Rep, ms(r.MaxIdleNs), r.MaxIdleAt, s.Digest, s.Full)
			} else {
				logf(t, "rep %d FAILED, full snapshot:\n%s", r.Rep, s.Full)
			}
		}
	}
	for _, r := range l.Reps {
		if r.OK {
			continue
		}
		msg := fmt.Sprintf("rep %d failed after %s ms (longest wait %s ms, began at byte %d): %s", r.Rep, ms(r.EndNs-r.StartNs), ms(r.MaxIdleNs), r.MaxIdleAt, r.Err)
		if n := len(r.Snaps); n > 0 {
			msg += "\n" + r.Snaps[n-1].Digest
		} else {
			msg += "\n(no stall-time snapshot: the failure was not a wait of " + pcfg.SnapAfter.String() + " or more)"
		}
		t.Error(sanitize(msg))
	}
}

func minOrDash(v []int64, lo int64) string {
	if len(v) == 0 {
		return "-"
	}
	return ms(lo)
}
