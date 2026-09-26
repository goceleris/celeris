//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/resource"
)

// celeris#662 and celeris#675: the lingering accept pause on io_uring.
//
// While TCP_DEFER_ACCEPT is on, which is the default, the kernel holds a
// connection whose handshake completed but which has sent nothing outside the
// accept queue, and promotes it into the queue about one second after its
// SYN. A pause that closed the listener before then reset it. The pause now
// clears the option on each listener, keeps the listener open and accepting
// for deferlinger.Linger after that clear, and only then drains and closes
// it.
//
// Every test here uses the default configuration (the option ON) and write-
// nothing clients, and every rescue test checks its own premises before it
// may pass: the clients were hidden (0 accepted within 200 ms), they were
// dialed well inside the kernel's one-second timer before the pause, they
// stay silent until every listener has closed, and net.ipv4.tcp_migrate_req
// reads 0 (with it on, the kernel would move deferred requests to another
// listener itself and a rescue would say nothing about the engine).
//
// The negative controls pass only by observing the loss: N1 with the linger
// at 0 (the old immediate close), N2 with the clear disabled, and N3 with
// the tcp_synack_retries=0 guard disabled at synack 0. They share the rigs of
// the tests they control, so a rig that cannot see a loss fails its control.
//
// They change package-level hooks in deferlinger, so none of them runs in
// parallel.

const (
	lingerIdleL662 = 8
	// lingerHiddenL662 is how long a silent client must stay unaccepted for
	// the premise "the option is on and hides it" to hold.
	lingerHiddenL662 = 200 * time.Millisecond
	// lingerRescueBoundL662: past this, from the dials to the pause (or to
	// the close, for the controls), the kernel's own one-second SYN-ACK
	// retransmit could have promoted the clients, and the run decides
	// nothing.
	lingerRescueBoundL662 = 900 * time.Millisecond
	// lingerArrivalEveryL662 spaces the silent arrivals through a pause.
	lingerArrivalEveryL662 = 15 * time.Millisecond
	// lingerAcceptBoundL662: an arrival after the clear enters the accept
	// queue at once, so the engine must accept it within this.
	lingerAcceptBoundL662 = 100 * time.Millisecond
	// lingerCloseMarginL662 excludes the arrivals whose handshake may still
	// have been in flight when the last listener closed. Resetting those is
	// inherent to closing a listen socket and is not what these tests pin.
	lingerCloseMarginL662 = 60 * time.Millisecond

	envRequirePremisesL662 = "CELERIS_REQUIRE_LINGER_PREMISES"
	envRequireSynack0L662  = "CELERIS_REQUIRE_SYNACK0"
)

// lingerRigL662 is one running engine under test.
type lingerRigL662 struct {
	e       *Engine
	addr    string
	port    int
	workers int
	// closed reports whether every worker has set listenFDClosed.
	closed func() bool
	cancel context.CancelFunc
	// exited is closed when Listen has returned.
	exited chan struct{}
	// accepted maps a client's local address to the time OnConnect fired
	// for it.
	accepted *sync.Map
}

// sockL662 is one LISTEN socket of this process on the port under test.
type sockL662 struct {
	cookie  uint64
	deferOn bool
	syncnt  int
}

// listenersL662 enumerates this process's LISTEN sockets bound to port via
// /proc/self/fd and reads SO_COOKIE, TCP_DEFER_ACCEPT and TCP_SYNCNT on each.
// A getsockopt on a descriptor number touches no memory the workers own. A
// number reused by a connection between the readlink and the getsockopt is
// filtered out by SO_ACCEPTCONN and the port.
func listenersL662(port int) ([]sockL662, error) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	var out []sockL662
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 1 {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		p := -1
		switch s := sa.(type) {
		case *unix.SockaddrInet4:
			p = s.Port
		case *unix.SockaddrInet6:
			p = s.Port
		}
		if p != port {
			continue
		}
		c, _ := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
		d, _ := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT)
		n, _ := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_SYNCNT)
		out = append(out, sockL662{cookie: c, deferOn: d != 0, syncnt: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cookie < out[j].cookie })
	return out, nil
}

func mustListenersL662(t *testing.T, port int) []sockL662 {
	t.Helper()
	ls, err := listenersL662(port)
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return ls
}

func describeL662(ls []sockL662) string {
	var b strings.Builder
	for i, s := range ls {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:defer=%v,syncnt=%d", s.cookie, s.deferOn, s.syncnt)
	}
	return "[" + b.String() + "]"
}

// requirePremiseL662 skips when an environment premise does not hold, or
// fails when CELERIS_REQUIRE_LINGER_PREMISES=1 says these tests are mandatory.
func requirePremiseL662(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if ok {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(envRequirePremisesL662) == "1" {
		t.Fatal(msg + " -- " + envRequirePremisesL662 + "=1 forbids skipping")
	}
	t.Skip(msg)
}

// requireMigrateReqZeroL662: with net.ipv4.tcp_migrate_req on, the kernel
// moves a closing listener's requests to another listener in the
// SO_REUSEPORT group, and a rescue would not be the engine's. A kernel
// without the sysctl has no migration at all.
func requireMigrateReqZeroL662(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_migrate_req")
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	v := strings.TrimSpace(string(b))
	requirePremiseL662(t, err == nil && v == "0",
		"net.ipv4.tcp_migrate_req reads %q (%v): the kernel would migrate deferred requests "+
			"itself, so a rescue here would say nothing about the engine", v, err)
}

// requireSynack0L662 runs a test only at net.ipv4.tcp_synack_retries=0.
// CELERIS_REQUIRE_SYNACK0=1 (the CI step that sets the sysctl) turns the skip
// into a failure.
func requireSynack0L662(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_synack_retries")
	v := strings.TrimSpace(string(b))
	if err == nil && v == "0" {
		return
	}
	msg := fmt.Sprintf("needs net.ipv4.tcp_synack_retries=0, reads %q (%v)", v, err)
	if os.Getenv(envRequireSynack0L662) == "1" {
		t.Fatal(msg + " -- " + envRequireSynack0L662 + "=1 forbids skipping")
	}
	t.Skip(msg)
}

// tcpDeferAcceptDropsL662 reads TcpExtTCPDeferAcceptDrop: the kernel bumps it
// each time it drops a bare handshake ACK because the listener defers. ok is
// false when the counter is not exposed.
func tcpDeferAcceptDropsL662() (v uint64, ok bool) {
	b, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return 0, false
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "TcpExt:") || !strings.HasPrefix(lines[i+1], "TcpExt:") {
			continue
		}
		names, vals := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		if len(names) != len(vals) {
			continue
		}
		for j, n := range names {
			if n == "TCPDeferAcceptDrop" {
				v, perr := strconv.ParseUint(vals[j], 10, 64)
				return v, perr == nil
			}
		}
	}
	return 0, false
}

func dialSilentL662(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	out := make([]net.Conn, 0, n)
	t.Cleanup(func() {
		for _, c := range out {
			_ = c.Close()
		}
	})
	for range n {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// writeAllL662 sends one request on every connection, concurrently, and
// names how each exchange ended.
func writeAllL662(conns []net.Conn) []string {
	out := make([]string, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			if _, err := c.Write([]byte(queuedReq662)); err != nil {
				out[i] = "write: " + err.Error()
				return
			}
			out[i] = readOutcome662(c)
		})
	}
	wg.Wait()
	return out
}

func tallyL662(o []string) map[string]int {
	m := map[string]int{}
	for _, s := range o {
		m[s]++
	}
	return m
}

func refusedL662(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// pauseAsyncL662 runs PauseAccept on its own goroutine and reports how long
// it took.
func pauseAsyncL662(e *Engine) <-chan time.Duration {
	ch := make(chan time.Duration, 1)
	go func() {
		s := time.Now()
		_ = e.PauseAccept()
		ch <- time.Since(s)
	}()
	return ch
}

type idleModeL662 int

const (
	idleLingerL662   idleModeL662 = iota // T1, and T4 at synack 0
	idleNoLingerL662                     // N1: linger 0, the old immediate close
	idleNoGuardL662                      // N3: the synack guard off, at synack 0
)

// runLingerIdleL662 is the rig of T1, T4, N1 and N3: eight clients complete
// their handshakes and send nothing, the pause starts while the kernel still
// holds them, and they write only after every listener has closed.
func runLingerIdleL662(t *testing.T, mode idleModeL662, synack0 bool) {
	requireMigrateReqZeroL662(t)
	switch mode {
	case idleNoLingerL662:
		old := deferlinger.SetLinger(0)
		t.Cleanup(func() { deferlinger.SetLinger(old) })
	case idleNoGuardL662:
		old := deferlinger.SetGuardEnabled(false)
		t.Cleanup(func() { deferlinger.SetGuardEnabled(old) })
	}
	r := startLingerL662(t)
	before := mustListenersL662(t, r.port)
	if len(before) == 0 {
		t.Fatal("premise: no listener found on the port")
	}
	for _, s := range before {
		if !s.deferOn {
			t.Fatalf("premise: a steady-state listener reads TCP_DEFER_ACCEPT off: %s", describeL662(before))
		}
	}
	base := r.e.Metrics()
	drops0, dropsOK := tcpDeferAcceptDropsL662()

	tDial := time.Now()
	idle := dialSilentL662(t, r.addr, lingerIdleL662)
	time.Sleep(lingerHiddenL662)
	hidden := r.e.Metrics().AcceptCount - base.AcceptCount
	drops1, _ := tcpDeferAcceptDropsL662()
	if hidden != 0 {
		t.Fatalf("premise: %d of %d silent clients were accepted within %v, want 0: the option "+
			"is not hiding them, so nothing below would test the pause", hidden, lingerIdleL662, lingerHiddenL662)
	}
	if dropsOK && drops1-drops0 < lingerIdleL662 {
		t.Fatalf("premise: TCPDeferAcceptDrop moved by %d over %d silent dials, want >= %d",
			drops1-drops0, lingerIdleL662, lingerIdleL662)
	}

	tPause := time.Now()
	if d := tPause.Sub(tDial); d >= lingerRescueBoundL662 {
		t.Fatalf("undecidable: %v from the dials to the pause is within reach of the kernel's "+
			"one-second SYN-ACK retransmit, which would promote the clients before the pause", d)
	}
	pdone := pauseAsyncL662(r.e)
	var during []sockL662
	if mode != idleNoLingerL662 {
		time.Sleep(300 * time.Millisecond)
		during = mustListenersL662(t, r.port)
	}
	var wall time.Duration
	select {
	case wall = <-pdone:
	case <-time.After(5 * time.Second):
		t.Fatal("PauseAccept did not return within 5s")
	}
	tReturn := time.Now()
	closedAtReturn := r.closed()
	acceptedAtReturn := r.e.Metrics().AcceptCount - base.AcceptCount

	tally := tallyL662(writeAllL662(idle))
	refused := refusedL662(r.addr)
	t.Logf("mode=%d synack0=%v workers=%d listenersBefore=%s hidden200ms=%d deferDrop=+%d "+
		"dialToPause=%v pauseWall=%v dialToClose=%v listenersDuring=%s closedAtReturn=%v "+
		"acceptedAtReturn=%d outcomes=%v laterDialRefused=%v deferlinger=%+v",
		mode, synack0, r.workers, describeL662(before), hidden, drops1-drops0,
		tPause.Sub(tDial), wall, tReturn.Sub(tDial), describeL662(during), closedAtReturn,
		acceptedAtReturn, tally, refused, deferlinger.Snapshot())

	switch mode {
	case idleLingerL662:
		if !closedAtReturn {
			t.Errorf("PauseAccept returned with a listener still open")
		}
		if wall < time.Second {
			t.Errorf("PauseAccept took %v, want >= 1s: it did not linger", wall)
		}
		if len(during) != len(before) {
			t.Errorf("listeners during the linger %s, before %s: one closed early",
				describeL662(during), describeL662(before))
		}
		for _, s := range during {
			if s.deferOn {
				t.Errorf("a lingering listener still reads TCP_DEFER_ACCEPT on: the clear did not happen")
			}
			if synack0 && s.syncnt != 1 {
				t.Errorf("at tcp_synack_retries=0 a lingering listener reads TCP_SYNCNT %d, want 1 "+
					"(the guard)", s.syncnt)
			}
		}
		if acceptedAtReturn != lingerIdleL662 {
			t.Errorf("%d of %d deferred clients had been accepted when PauseAccept returned",
				acceptedAtReturn, lingerIdleL662)
		}
		if tally["200"] != lingerIdleL662 {
			t.Errorf("served %d of %d clients whose handshake completed before the pause "+
				"(outcomes %v): a pause must not reset a connected client that has not sent "+
				"its request yet (celeris#662, celeris#675)", tally["200"], lingerIdleL662, tally)
		}
		if !refused {
			t.Errorf("a dial after PauseAccept returned connected: the pause no longer stops accepting")
		}
	case idleNoLingerL662:
		if d := tReturn.Sub(tDial); d >= lingerRescueBoundL662 {
			t.Fatalf("undecidable: %v from the dials to the close; the kernel's retransmit may "+
				"have promoted the clients", d)
		}
		if tally["RESET"] < lingerIdleL662-1 {
			t.Errorf("negative control N1 did not lose: with the linger at 0 the close must reset "+
				"the deferred clients (outcomes %v, want RESET >= %d). A rig that cannot see the "+
				"loss cannot show the linger prevents it", tally, lingerIdleL662-1)
		}
	case idleNoGuardL662:
		if tally["RESET"] != lingerIdleL662 {
			t.Errorf("negative control N3 did not lose: at tcp_synack_retries=0 without the "+
				"TCP_SYNCNT guard the kernel must drop every deferred client at its first timer "+
				"(outcomes %v, want RESET %d)", tally, lingerIdleL662)
		}
	}
}

// TestPauseAcceptServesDeferredIdleConnections (T1): with the default
// configuration, clients that completed their handshake before PauseAccept
// but had not sent a byte are accepted during the linger and served; the pause
// lingers at least a second and still ends with every listener closed.
func TestPauseAcceptServesDeferredIdleConnections(t *testing.T) {
	runLingerIdleL662(t, idleLingerL662, false)
}

// TestPauseAcceptNoLingerResetsDeferredIdleConnections (N1) is T1 with the
// linger at 0, which is how the pause behaved before celeris#662's fix. It
// passes only by observing the resets.
func TestPauseAcceptNoLingerResetsDeferredIdleConnections(t *testing.T) {
	runLingerIdleL662(t, idleNoLingerL662, false)
}

// TestPauseAcceptServesDeferredIdleConnectionsSynackRetriesZero (T4) is T1 at
// net.ipv4.tcp_synack_retries=0, where clearing the option would make the
// kernel drop every deferred client at its first timer; the pausing listeners
// get TCP_SYNCNT=1 and the clients are served.
func TestPauseAcceptServesDeferredIdleConnectionsSynackRetriesZero(t *testing.T) {
	requireSynack0L662(t)
	runLingerIdleL662(t, idleLingerL662, true)
}

// TestPauseAcceptNoGuardLosesAtSynackRetriesZero (N3) is T4 with the guard
// disabled. It passes only by observing the loss the guard prevents.
func TestPauseAcceptNoGuardLosesAtSynackRetriesZero(t *testing.T) {
	requireSynack0L662(t)
	runLingerIdleL662(t, idleNoGuardL662, true)
}

type arrivalL662 struct {
	done  time.Time
	conn  net.Conn
	local string
}

type arrivalsResultL662 struct {
	tPause, tCleared, tClosed time.Time
	preTally                  map[string]int
	connected                 int
	lostEarly                 []time.Duration // dial-done offsets from the pause call
	slowAccepts               []string
	checkedAccepts            int
}

// runLingerArrivalsL662 is the rig of T2, T3 and N2. Eight silent clients
// connect before the pause; a silent client dials every
// lingerArrivalEveryL662 from 200 ms before the PauseAccept call until 60 ms
// after every listener has closed. Every client stays silent until then, and
// then writes one request. An arrival is lost when it connected but got no
// 200, and it is counted only if its dial completed more than
// lingerCloseMarginL662 before the close.
func runLingerArrivalsL662(t *testing.T) arrivalsResultL662 {
	requireMigrateReqZeroL662(t)
	r := startLingerL662(t)
	var res arrivalsResultL662

	var mu sync.Mutex
	var arr []arrivalL662
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(lingerArrivalEveryL662)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			c, err := net.DialTimeout("tcp", r.addr, time.Second)
			if err != nil {
				continue // refused: every listener has closed
			}
			a := arrivalL662{done: time.Now(), conn: c, local: c.LocalAddr().String()}
			mu.Lock()
			arr = append(arr, a)
			mu.Unlock()
		}
	})
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range arr {
			_ = a.conn.Close()
		}
	})

	time.Sleep(100 * time.Millisecond)
	pre := dialSilentL662(t, r.addr, lingerIdleL662)
	time.Sleep(100 * time.Millisecond)

	// Witness of the clear: the first moment every listener on the port reads
	// the option off.
	cleared := make(chan time.Time, 1)
	samplerStop := make(chan struct{})
	var swg sync.WaitGroup
	swg.Go(func() {
		for {
			select {
			case <-samplerStop:
				return
			default:
			}
			if ls, err := listenersL662(r.port); err == nil && len(ls) > 0 {
				all := true
				for _, s := range ls {
					if s.deferOn {
						all = false
						break
					}
				}
				if all {
					cleared <- time.Now()
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	})

	res.tPause = time.Now()
	_ = r.e.PauseAccept()
	for dl := time.Now().Add(2 * time.Second); !r.closed() && time.Now().Before(dl); {
		time.Sleep(time.Millisecond)
	}
	if !r.closed() {
		t.Fatal("a listener never closed after PauseAccept")
	}
	res.tClosed = time.Now()
	close(samplerStop)
	swg.Wait()
	select {
	case res.tCleared = <-cleared:
	default:
	}
	time.Sleep(60 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	all := append([]arrivalL662(nil), arr...)
	mu.Unlock()
	res.connected = len(all)
	conns := make([]net.Conn, 0, len(pre)+len(all))
	conns = append(conns, pre...)
	for _, a := range all {
		conns = append(conns, a.conn)
	}
	out := writeAllL662(conns)
	res.preTally = tallyL662(out[:len(pre)])
	cutoff := res.tClosed.Add(-lingerCloseMarginL662)
	for i, a := range all {
		if out[len(pre)+i] != "200" && a.done.Before(cutoff) {
			res.lostEarly = append(res.lostEarly, a.done.Sub(res.tPause).Round(time.Millisecond))
		}
		if res.tCleared.IsZero() || !a.done.After(res.tCleared.Add(5*time.Millisecond)) || !a.done.Before(cutoff) {
			continue
		}
		res.checkedAccepts++
		v, ok := r.accepted.Load(a.local)
		switch {
		case !ok:
			res.slowAccepts = append(res.slowAccepts, a.local+": never accepted")
		case v.(time.Time).Sub(a.done) > lingerAcceptBoundL662:
			res.slowAccepts = append(res.slowAccepts,
				fmt.Sprintf("%s: accepted %v after its dial completed", a.local, v.(time.Time).Sub(a.done)))
		}
	}
	var clearOff time.Duration
	if !res.tCleared.IsZero() {
		clearOff = res.tCleared.Sub(res.tPause)
	}
	t.Logf("workers=%d pre=%v arrivalsConnected=%d clearSeenAt=+%v closeAt=+%v outcomes=%v "+
		"lostEarly=%d %v acceptChecked=%d slow=%d deferlinger=%+v",
		r.workers, res.preTally, res.connected, clearOff.Round(time.Millisecond),
		res.tClosed.Sub(res.tPause).Round(time.Millisecond), tallyL662(out[len(pre):]),
		len(res.lostEarly), res.lostEarly, res.checkedAccepts, len(res.slowAccepts),
		deferlinger.Snapshot())
	if res.preTally["200"] != len(pre) {
		t.Errorf("clients connected before the pause: served %d of %d (%v)",
			res.preTally["200"], len(pre), res.preTally)
	}
	return res
}

// TestPauseAcceptServesArrivalsDuringLinger (T2): connections that arrive
// during the linger are neither deferred nor lost. After the clear each one is
// accepted within lingerAcceptBoundL662, and none that connected before the
// last moments of the close is reset.
func TestPauseAcceptServesArrivalsDuringLinger(t *testing.T) {
	res := runLingerArrivalsL662(t)
	if res.tCleared.IsZero() {
		t.Fatal("no moment was seen with every listener's TCP_DEFER_ACCEPT off: the clear did not happen")
	}
	if len(res.lostEarly) != 0 {
		t.Errorf("%d arrivals that connected before the close were reset (dial-done offsets %v)",
			len(res.lostEarly), res.lostEarly)
	}
	if res.checkedAccepts < 20 {
		t.Errorf("only %d arrivals landed between the clear and the close: the linger was not "+
			"exercised", res.checkedAccepts)
	}
	if len(res.slowAccepts) != 0 {
		t.Errorf("%d of %d arrivals after the clear were not accepted within %v: %v",
			len(res.slowAccepts), res.checkedAccepts, lingerAcceptBoundL662, res.slowAccepts)
	}
}

// TestPauseAcceptNoClearLosesArrivals (N2) is T2 with the clear disabled: the
// listener lingers with the option still on, so the arrivals of its last
// second are deferred past the close and reset. It passes only by observing
// that loss.
func TestPauseAcceptNoClearLosesArrivals(t *testing.T) {
	old := deferlinger.SetClearOnPause(false)
	t.Cleanup(func() { deferlinger.SetClearOnPause(old) })
	res := runLingerArrivalsL662(t)
	if len(res.lostEarly) < 10 {
		t.Errorf("negative control N2 lost %d arrivals, want >= 10: with the option left on, the "+
			"arrivals of the linger's last second are promoted after the close. A rig that cannot "+
			"see that cannot show the clear prevents it", len(res.lostEarly))
	}
}

// TestPauseAcceptLingerAnchoredAtClear (T3): the linger is measured from each
// listener's own clear, not from the pause call. The test delays the moment
// the loops act on the pause by 800 ms; a deadline taken at the call would
// close the listeners 700 ms after the clear, before the kernel promotes the
// clients that connected in the second before it.
func TestPauseAcceptLingerAnchoredAtClear(t *testing.T) {
	old := deferlinger.SetObserveDelay(800 * time.Millisecond)
	t.Cleanup(func() { deferlinger.SetObserveDelay(old) })
	res := runLingerArrivalsL662(t)
	if res.tCleared.IsZero() || res.tCleared.Sub(res.tPause) < 700*time.Millisecond {
		t.Fatalf("premise: the clear was seen at +%v, want >= 700ms: the observation delay did "+
			"not take effect", res.tCleared.Sub(res.tPause))
	}
	if len(res.lostEarly) != 0 {
		t.Errorf("%d arrivals that connected before the close were reset (dial-done offsets %v): "+
			"the linger was not measured from the clear", len(res.lostEarly), res.lostEarly)
	}
}

// TestResumeDuringLingerRestoresDeferAccept (T5): a ResumeAccept that lands
// during the linger ends the pause on the same listeners, never closed and
// never re-created, and puts TCP_DEFER_ACCEPT back on them.
func TestResumeDuringLingerRestoresDeferAccept(t *testing.T) {
	r := startLingerL662(t)
	before := mustListenersL662(t, r.port)
	if len(before) == 0 {
		t.Fatal("premise: no listener found on the port")
	}
	pdone := pauseAsyncL662(r.e)
	time.Sleep(300 * time.Millisecond)
	during := mustListenersL662(t, r.port)
	tResume := time.Now()
	if err := r.e.ResumeAccept(); err != nil {
		t.Fatalf("ResumeAccept: %v", err)
	}
	var wall time.Duration
	select {
	case wall = <-pdone:
	case <-time.After(3 * time.Second):
		t.Fatal("PauseAccept did not return after ResumeAccept")
	}
	returnedAfterResume := time.Since(tResume)
	var after []sockL662
	for dl := time.Now().Add(time.Second); ; {
		after = mustListenersL662(t, r.port)
		on := len(after) == len(before)
		for _, s := range after {
			on = on && s.deferOn
		}
		if on || time.Now().After(dl) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	base := r.e.Metrics()
	fresh := dialSilentL662(t, r.addr, 1)
	time.Sleep(lingerHiddenL662)
	hidden := r.e.Metrics().AcceptCount - base.AcceptCount
	tally := tallyL662(writeAllL662(fresh))
	t.Logf("workers=%d before=%s during=%s after=%s pauseWall=%v returnedAfterResume=%v "+
		"freshAccepted200ms=%d fresh=%v", r.workers, describeL662(before), describeL662(during),
		describeL662(after), wall, returnedAfterResume, hidden, tally)

	if len(during) != len(before) {
		t.Fatalf("premise: listeners %s during the linger, %s before", describeL662(during), describeL662(before))
	}
	for _, s := range during {
		if s.deferOn {
			t.Fatalf("premise: a listener reads TCP_DEFER_ACCEPT on 300 ms into the pause, so it was " +
				"not lingering")
		}
	}
	if returnedAfterResume > 500*time.Millisecond {
		t.Errorf("PauseAccept returned %v after ResumeAccept: a withdrawn pause must not be waited out",
			returnedAfterResume)
	}
	if len(after) != len(before) {
		t.Errorf("listeners %s after the resume, %s before", describeL662(after), describeL662(before))
	}
	for i := range after {
		if i < len(before) && after[i].cookie != before[i].cookie {
			t.Errorf("a listener was re-created across a resume during the linger (cookie %d -> %d)",
				before[i].cookie, after[i].cookie)
		}
		if !after[i].deferOn {
			t.Errorf("after the resume a listener reads TCP_DEFER_ACCEPT off: the option was not restored")
		}
	}
	if hidden != 0 {
		t.Errorf("a fresh silent client was accepted within %v of the resume: the option is not in effect",
			lingerHiddenL662)
	}
	if tally["200"] != 1 {
		t.Errorf("a fresh client after the resume was not served: %v", tally)
	}
}

// TestShutdownDuringLingerIsPrompt (T6): a shutdown during the linger ends it.
// Each worker's shutdown sets listenFDClosed, so a PauseAccept that is
// waiting returns at once instead of running out its own bound, and Listen
// returns.
func TestShutdownDuringLingerIsPrompt(t *testing.T) {
	r := startLingerL662(t)
	_ = dialSilentL662(t, r.addr, 4)
	pdone := pauseAsyncL662(r.e)
	time.Sleep(300 * time.Millisecond)
	during := mustListenersL662(t, r.port)
	tCancel := time.Now()
	r.cancel()
	var pauseAfterCancel time.Duration
	select {
	case <-pdone:
		pauseAfterCancel = time.Since(tCancel)
	case <-time.After(5 * time.Second):
		t.Fatal("PauseAccept did not return within 5s of the shutdown")
	}
	var listenAfterCancel time.Duration
	select {
	case <-r.exited:
		listenAfterCancel = time.Since(tCancel)
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return within 5s of the shutdown")
	}
	after := mustListenersL662(t, r.port)
	t.Logf("workers=%d during=%s pauseReturnedAfterCancel=%v listenReturnedAfterCancel=%v after=%s",
		r.workers, describeL662(during), pauseAfterCancel, listenAfterCancel, describeL662(after))
	if len(during) == 0 {
		t.Fatal("premise: no listener was open 300 ms into the pause, so it was not lingering")
	}
	for _, s := range during {
		if s.deferOn {
			t.Fatal("premise: a listener reads TCP_DEFER_ACCEPT on 300 ms into the pause, so it was " +
				"not lingering")
		}
	}
	if pauseAfterCancel > 500*time.Millisecond {
		t.Errorf("PauseAccept returned %v after the shutdown began: an exited worker must "+
			"release the pause's wait", pauseAfterCancel)
	}
	if len(after) != 0 {
		t.Errorf("listeners still open after Listen returned: %s", describeL662(after))
	}
}

// startLingerL662 starts an io_uring engine with the default configuration
// (TCP_DEFER_ACCEPT on), asking for two workers, and records when each
// connection is accepted. Under a low RLIMIT_MEMLOCK the engine starts fewer
// workers; the count is logged by every test.
func startLingerL662(t *testing.T) *lingerRigL662 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	accepted := &sync.Map{}
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.DiscardHandler),
		OnConnect: func(ra string) { accepted.LoadOrStore(ra, time.Now()) },
	}, idleRespHandler662{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	var listenErr error
	go func() {
		listenErr = e.Listen(ctx)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})
	for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			break
		}
		select {
		case <-exited:
			skipOrFail656(t, "io_uring Listen failed here: %v", listenErr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() == 0 {
		t.Fatal("engine did not bind with workers")
	}
	ws := workers662(e)
	return &lingerRigL662{
		e: e, addr: addr, port: port, workers: len(ws),
		closed: func() bool {
			for _, w := range ws {
				if !w.listenFDClosed.Load() {
					return false
				}
			}
			return true
		},
		cancel: cancel, exited: exited, accepted: accepted,
	}
}
