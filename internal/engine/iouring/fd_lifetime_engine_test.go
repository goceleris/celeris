//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#657 face 2 end to end: a real io_uring engine, keep-alive clients
// that never stop sending, and a hand-off across StartTransplant. On the base
// every hand-off leaves the connection's next recv armed, and some of those
// recvs take a request the client then waits on forever.

// startFDLEngine runs an io_uring engine on a free loopback port. Workers is
// 2 (the minimum Resources accepts); RLIMIT_MEMLOCK decides how many actually
// start (one at 8 MiB), and the count is logged as workers=N so a run can be
// checked against the shape it was registered for.
func startFDLEngine(t *testing.T, h stream.Handler, mut func(*resource.Config)) (*Engine, string) {
	t.Helper()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick port: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg := resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 2},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if mut != nil {
			mut(&cfg)
		}
		return New(cfg, h)
	})
	addr := e.Addr().String()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	t.Logf("celeris657 engine workers=%d", e.NumWorkers())
	return e, addr
}

// servingTarget is a stand-in epoll engine: every adopted descriptor is served
// as HTTP/1 keep-alive by a goroutine, so a client whose conn was handed off
// keeps getting answers — unless a recv left on the source took its request.
type servingTarget struct {
	adopted atomic.Int64
	onAdopt func() // runs first, on the source worker's thread
	wg      sync.WaitGroup
	mu      sync.Mutex
	conns   []net.Conn
	// byPort is what the target saw of each adopted conn, keyed by the
	// client's local port (this side's peer port), for attribute.
	byPort map[int]*adoptedConn
}

// adoptedConn counts what the target's serving goroutine did on one conn.
type adoptedConn struct {
	adoptedAt  time.Time
	lastRead   atomic.Int64 // sequence number of the last request the target read
	lastSent   atomic.Int64 // sequence number of the last response the target wrote
	lastSentAt atomic.Int64 // UnixNano at which that response was written
}

// attribute says, for each client that ended on a read timeout, where its
// request stopped; see celeris#892 (from #816 item 4). The target's counts against the
// client's, by the sequence number each request carries in its path: never
// adopted (the conn was still the source's, or was closed by it); the last
// request read is older than the last the client wrote (that request never
// reached the target's reader: eaten by the source or lost in the hand-off);
// the last request was read and not answered (the target's goroutine stalled);
// or it was read and answered (the response was written and the client did
// not read it within its deadline: a stall of the client or the host, not of
// the engine), or it was answered, but only after the client's deadline had
// passed (the target itself was late).
func (s *servingTarget) attribute(ts []clientTimeout) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var detail []string
	for _, ct := range ts {
		ac := s.byPort[ct.port]
		var cls string
		switch {
		case ac == nil:
			cls = "never_adopted"
		case ac.lastRead.Load() < ct.sent:
			cls = "request_never_reached_target"
		case ac.lastSent.Load() < ct.sent:
			cls = "target_read_not_answered"
		case ac.lastSentAt.Load() > ct.deadline.UnixNano():
			cls = "target_answered_after_client_deadline"
		default:
			cls = "target_answered_client_missed_it"
		}
		counts[cls]++
		if len(detail) < 8 {
			if ac == nil {
				detail = append(detail, fmt.Sprintf("port=%d sent=%d answered=%d %s", ct.port, ct.sent, ct.answered, cls))
			} else {
				detail = append(detail, fmt.Sprintf("port=%d client_sent=%d client_answered=%d target_last_read=%d target_last_answered=%d adopted_%s_ago %s",
					ct.port, ct.sent, ct.answered, ac.lastRead.Load(), ac.lastSent.Load(), time.Since(ac.adoptedAt).Round(time.Millisecond), cls))
			}
		}
	}
	return fmt.Sprintf("%s; first: [%s]", classes(toInt64(counts)), strings.Join(detail, "; "))
}

func toInt64(m map[string]int) map[string]int64 {
	o := make(map[string]int64, len(m))
	for k, v := range m {
		o[k] = int64(v)
	}
	return o
}

func (s *servingTarget) AdoptConn(fd int, _ engine.Carryover) error {
	if s.onAdopt != nil {
		s.onAdopt()
	}
	f := os.NewFile(uintptr(fd), "adopted")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil // fd closed above; nothing for the source to reclaim
	}
	s.adopted.Add(1)
	ac := &adoptedConn{adoptedAt: time.Now()}
	s.mu.Lock()
	s.conns = append(s.conns, c)
	if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if s.byPort == nil {
			s.byPort = map[int]*adoptedConn{}
		}
		s.byPort[ra.Port] = ac
	}
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		br := bufio.NewReader(c)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			if n, err := strconv.ParseInt(strings.TrimPrefix(req.URL.Path, "/"), 10, 64); err == nil {
				ac.lastRead.Store(n)
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
			if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\ncontent-type: text/plain\r\ncontent-length: 2\r\n\r\nok")); err != nil {
				return
			}
			ac.lastSentAt.Store(time.Now().UnixNano())
			ac.lastSent.Store(ac.lastRead.Load())
		}
	}()
	return nil
}

func (s *servingTarget) close() {
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// refusingTarget refuses every hand-off, so the source must keep (reclaim)
// each connection it offers.
type refusingTarget struct{ refused atomic.Int64 }

func (r *refusingTarget) AdoptConn(int, engine.Carryover) error {
	r.refused.Add(1)
	return errors.New("refusingTarget: no")
}

// loadResult is what the clients saw.
type loadResult struct {
	ok   int64
	errs int64
	slow int64 // replies that took longer than keepAliveSlowReply but came
	// canary is the longest a 10 ms ticker in the test process was late
	// while the load ran: a stall of the whole process (the host starved it)
	// shows here, a delay of the engine alone does not.
	canary  time.Duration
	byClass map[string]int64
	// timeouts is one entry per client that ended on a read timeout, so a
	// failure can be attributed to the conn that stalled; see
	// servingTarget.attribute and celeris#892 (from #816 item 4).
	timeouts []clientTimeout
}

// clientTimeout is what a keep-alive client knew when its read timed out:
// the local port (the key servingTarget knows the conn by, as the peer port),
// how many requests it had written, how many it had read a response to, and
// the deadline of the read that failed.
type clientTimeout struct {
	port     int
	sent     int64
	answered int64
	deadline time.Time // when the read of the last request's reply gave up
}

// keepAliveCanaryStall is how late the process's own 10 ms ticker has to have
// been, at its worst, for a slow reply to be put down to the host rather than
// the engine.
const keepAliveCanaryStall = 500 * time.Millisecond

// slowRepliesAreSuspect says whether res holds a reply slower than
// keepAliveSlowReply that the test process cannot blame on itself being
// starved: the canary ticker, which runs beside the clients, was never more
// than keepAliveCanaryStall late, so a reply that took over a second was
// delayed after it left the clients.
func slowRepliesAreSuspect(res loadResult) bool {
	return res.slow > 0 && res.canary < keepAliveCanaryStall
}

// keepAliveReadTimeout is how long a keep-alive client waits for a reply
// before it counts the request as lost, and keepAliveSlowReply is the wait
// after which a reply that did come is counted as slow (loadResult.slow) and
// logged. The wait was a flat 1 s, so a host that starved the process for a
// second (the Docker VM shared with other containers under -race: 3,614 and
// 3,638 requests in the load window against a median of 67,167, and the
// conns' target had answered every request they timed out on, celeris#892
// (from #816 item 4)) failed the test with read_timeout and nothing to attribute. A
// request that is lost never gets a reply, so the longer wait still fails it,
// 10 s later.
var (
	keepAliveReadTimeout = 10 * time.Second
	keepAliveSlowReply   = time.Second
)

// runKeepAliveLoad drives n keep-alive clients against addr, each sending one
// request at a time with a keepAliveReadTimeout read deadline, until stop is
// closed. A request the server never answers surfaces as that client's read
// timeout (the wait for it ends keepAliveReadTimeout after the request); the
// client then stops, so each lost request is exactly one error.
func runKeepAliveLoad(t *testing.T, addr string, n int, stop <-chan struct{}) func() loadResult {
	t.Helper()
	return runKeepAliveLoadPaced(t, addr, n, stop, 0, keepAliveReadTimeout)
}

// runKeepAliveLoadPaced is runKeepAliveLoad with think between a client's
// answered request and its next (0: none, back to back). A worker serving
// clients that never wait finds a completion ready at every pass of its loop
// and takes no wait at all, or only the one that has sends to wait for; with
// think the clients leave it idle between requests, and the wait it takes
// then is the timed one (celeris#884 item 49, the test of that wait).
// readTimeout is how long a client waits for each reply: runKeepAliveLoad
// passes keepAliveReadTimeout, and a test that relies on a short deadline to
// catch a stopped worker passes its own (warmLoad884: 1 s).
func runKeepAliveLoadPaced(t *testing.T, addr string, n int, stop <-chan struct{}, think, readTimeout time.Duration) func() loadResult {
	t.Helper()
	canaryStop := make(chan struct{})
	var canaryMax atomic.Int64
	go func() {
		const period = 10 * time.Millisecond
		tk := time.NewTicker(period)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-canaryStop:
				return
			case <-tk.C:
				now := time.Now()
				if late := int64(now.Sub(last) - period); late > canaryMax.Load() {
					canaryMax.Store(late)
				}
				last = now
			}
		}
	}()
	var ok, errsN, slowN atomic.Int64
	var mu sync.Mutex
	byClass := map[string]int64{}
	var timeouts []clientTimeout
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			var sent, answered int64
			var deadline time.Time
			fail := func(err error) {
				errsN.Add(1)
				cls := "other"
				switch {
				case errors.Is(err, os.ErrDeadlineExceeded):
					cls = "read_timeout"
					if la, ok := c.LocalAddr().(*net.TCPAddr); ok {
						mu.Lock()
						timeouts = append(timeouts, clientTimeout{port: la.Port, sent: sent, answered: answered, deadline: deadline})
						mu.Unlock()
					}
				case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
					cls = "eof"
				case errors.Is(err, syscall.ECONNRESET):
					cls = "reset"
				}
				mu.Lock()
				byClass[cls]++
				mu.Unlock()
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The request path is its sequence number on this conn,
				// which servingTarget.attribute reads back (the handlers
				// ignore the path).
				if _, err := c.Write([]byte("GET /" + strconv.FormatInt(sent+1, 10) + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
					fail(err)
					return
				}
				sent++
				asked := time.Now()
				deadline = asked.Add(readTimeout)
				_ = c.SetReadDeadline(deadline)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					fail(err)
					return
				}
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					fail(err)
					return
				}
				if time.Since(asked) > keepAliveSlowReply {
					slowN.Add(1)
				}
				answered++
				ok.Add(1)
				if think > 0 {
					select {
					case <-stop:
						return
					case <-time.After(think):
					}
				}
			}
		}()
	}
	return func() loadResult {
		wg.Wait()
		close(canaryStop)
		mu.Lock()
		defer mu.Unlock()
		cp := map[string]int64{}
		for k, v := range byClass {
			cp[k] = v
		}
		return loadResult{ok: ok.Load(), errs: errsN.Load(), slow: slowN.Load(), canary: time.Duration(canaryMax.Load()), byClass: cp, timeouts: append([]clientTimeout(nil), timeouts...)}
	}
}

func classes(m map[string]int64) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k + "=" + strconv.FormatInt(m[k], 10) + " ")
	}
	return strings.TrimSpace(b.String())
}

// transplantUnderLoad starts n clients, calls StartTransplant(tgt) after
// warm, keeps the load up for after, stops it and returns what the clients
// saw. The stale CQEs of any stolen request arrive within the load window;
// the final wait lets the last of them land before the counters are read.
// It logs every celeris#657 counter, and fails the test if one of the three
// that must stay 0 moved (TransplantDoubleClaim, TransplantReapFailed,
// TransplantHoldRescued; celeris#681 R4).
func transplantUnderLoad(t *testing.T, e *Engine, addr string, n int, tgt engine.TransplantTarget,
	warm, after time.Duration,
) loadResult {
	t.Helper()
	stop := make(chan struct{})
	wait := runKeepAliveLoad(t, addr, n, stop)
	time.Sleep(warm)
	e.StartTransplant(tgt)
	time.Sleep(after)
	close(stop)
	res := wait()
	e.StopTransplant()
	time.Sleep(100 * time.Millisecond)
	t.Logf("celeris657 load conns=%d ok=%d slow=%d errs=%d classes=[%s] W1T=%d W1U=%d W1C=%d W2=%d "+
		"held=%d reaps=%d misses=%d rescued=%d doubleclaim=%d detached=%d "+
		"claimdeferred=%d reapfailed=%d reapunsupported=%d",
		n, res.ok, res.slow, res.errs, classes(res.byClass),
		e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
		e.metrics.handoffLoss.staleRecvDataUnattributed.Load(),
		e.metrics.handoffLoss.staleRecvDataClosed.Load(),
		e.metrics.handoffLoss.handoffInFlight.Load(),
		metricOr(e, "TransplantHeld"), metricOr(e, "TransplantReaps"), metricOr(e, "TransplantReapMisses"),
		metricOr(e, "TransplantHoldRescued"), metricOr(e, "TransplantDoubleClaim"),
		e.metrics.transplantDetached.Load(),
		metricOr(e, "TransplantClaimDeferred"), metricOr(e, "TransplantReapFailed"),
		metricOr(e, "TransplantReapUnsupported"))
	for _, name := range []string{"TransplantDoubleClaim", "TransplantReapFailed", "TransplantHoldRescued"} {
		if n := metric(t, e, name); n != 0 {
			t.Errorf("%s = %d, want 0 (a hand-off counter that must stay 0)", name, n)
		}
	}
	return res
}

// metricOr is metric for a log line: -1 when the tree has no such field.
func metricOr(e *Engine, name string) int64 {
	v := reflect.ValueOf(e.Metrics()).FieldByName(name)
	if !v.IsValid() {
		return -1
	}
	return int64(v.Uint())
}

// TestHandoffHasNothingInFlight: at every hand-off, nothing may still be in
// flight on the connection (TransplantHandoffInFlight does not move), and no
// stale recv data may appear afterwards, with 128 busy keep-alives across a
// StartTransplant. Base: every hand-off is made with the conn's recv armed.
// The sync conns leave through tryTransplant (a held response's SEND
// completion, or a reap); the async ones are promoted to a dispatch goroutine
// that claims its own hand-off at its park, and leave through
// finishAsyncTransplant, which reaps the recv the feed path armed.
//
// Where the probe did not find the cancel flags a promoted async conn cannot
// be reaped, so it is never offered for the hand-off and stays on io_uring
// (asyncTransplantEligible, celeris#681 R1): there the async subtest wants
// none of them handed off, and no client error. It used to want all of them
// on every kernel, so it failed on 5.15.0-191 ("0 of 128 busy conns were
// handed off"; celeris#681 N4). A kernel that rejects the flags (every one
// before 5.19) now gets no io_uring engine at all (celeris#682), so the case
// is left to a 5.19+ kernel whose probe got no answer.
// async_without_cancel_flags runs it on any such kernel: the engine's probe
// is made to get no answer (runAsyncCancelProbe).
//
// Which of the two the async subtest wants is decided by the RUNNING KERNEL,
// not by the probe's answer alone (celeris#681 M1). From 5.19 the kernel has
// the flags, so there the reap must work and all 128 must be handed off; if
// the engine's reap is off on such a kernel the subtest fails, naming the
// probe's class from New's own record. Only async_without_cancel_flags may
// take the weak branch: a kernel that predates 5.19 gets no io_uring engine
// unless its probe found the flags (celeris#682). Chosen from the probe
// alone, a probe that got no
// answer — an EMFILE or ENOMEM in its private ring, which N1 made a per-New
// event — silently turned "all 128 handed off" into "none handed off", which
// such a run passes trivially; this subtest is the registered killer of
// mutants m01 and m04, so the mutation claim inherited that weakness.
func TestHandoffHasNothingInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		async   bool
		noFlags bool
	}{{"sync", false, false}, {"async", true, false}, {"async_without_cancel_flags", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			const conns = 128
			var h stream.Handler = transplantTestHandler{}
			var logs lockedBuffer
			var mut func(*resource.Config)
			if tc.async {
				h = asyncRouteHandler{}
				// New's own records, so a failure can name the probe's
				// class without probing a second time (celeris#681 M1).
				mut = func(c *resource.Config) {
					c.AsyncHandlers = true
					c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				}
			}
			if tc.noFlags {
				saved := runAsyncCancelProbe
				// No answer, not a rejection: New refuses io_uring on a
				// kernel that rejects the flags (celeris#682), and from 5.19
				// a probe with no answer builds the engine with the reap off.
				runAsyncCancelProbe = func() (asyncCancelProbe, string) {
					return asyncCancelNoAnswer, "celeris681 forced: the probe got no answer"
				}
				resetAsyncCancelProbeCache()
				t.Cleanup(func() {
					runAsyncCancelProbe = saved
					resetAsyncCancelProbeCache() // the next New probes the kernel again
				})
			}
			e, addr := startFDLEngine(t, h, mut)
			if tc.noFlags && e.asyncCancelFlags {
				t.Fatal("New turned the reap on although the probe got no answer")
			}
			// celeris#681 M1: a kernel from 5.19 has IORING_ASYNC_CANCEL
			// flags, so the reap must be on there and the strong branch
			// below is the one that runs. Whatever kept it off — a probe
			// with no answer, an answer the probe does not recognise, or a
			// kernel that claims 5.19 and rejects them — is a failure of
			// this subtest, not a reason to want less of it.
			kernelHasFlags := e.profile.KernelMajor > 5 ||
				(e.profile.KernelMajor == 5 && e.profile.KernelMinor >= 19)
			if tc.async {
				t.Logf("celeris681 M1 kernel=%s (%d.%d, a version with the flags: %v) forced_no_flags=%v reap=%v; %s",
					e.profile.KernelVersion, e.profile.KernelMajor, e.profile.KernelMinor, kernelHasFlags,
					tc.noFlags, e.asyncCancelFlags, asyncCancelProbeRecords(t, &logs))
			}
			if tc.async && !tc.noFlags && kernelHasFlags && !e.asyncCancelFlags {
				t.Fatalf("the engine's cancel-flags probe did not find the flags accepted on kernel %s (%d.%d), "+
					"which has had them since 5.19, so the hand-off's reap is off and no promoted async conn is "+
					"offered for it: %s. A probe with no answer is the probe failing, not the kernel answering "+
					"(its private ring can fail with EMFILE or ENOMEM, and celeris#681 N1 made that a per-New "+
					"event); a rejection here is a kernel whose version claims the flags and does not have them. "+
					"Either way the check below must stay 'all %d handed off' on this kernel and must not weaken "+
					"to 'none handed off', which any run with the reap off passes trivially (celeris#681 M1). "+
					"The no-flags path is async_without_cancel_flags, which forces it on every kernel.",
					e.profile.KernelVersion, e.profile.KernelMajor, e.profile.KernelMinor,
					asyncCancelProbeRecords(t, &logs), conns)
			}
			var maxSeen atomic.Uint64
			tgt := &servingTarget{}
			tgt.onAdopt = func() {
				if v := e.metrics.handoffLoss.handoffInFlight.Load(); v > maxSeen.Load() {
					maxSeen.Store(v)
				}
			}
			defer tgt.close()
			res := transplantUnderLoad(t, e, addr, conns, tgt, 300*time.Millisecond, 1500*time.Millisecond)
			if tc.async {
				if n := e.Metrics().AsyncPromotedConns; n == 0 {
					t.Fatal("no conn was promoted to async dispatch: the async path was not exercised")
				}
			}
			if v := maxSeen.Load(); v != 0 {
				t.Errorf("TransplantHandoffInFlight read %d at a hand-off, want 0 at every one: a conn "+
					"left with an op still able to resolve its fd", v)
			}
			if tr, un := e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
				e.metrics.handoffLoss.staleRecvDataUnattributed.Load(); tr != 0 || un != 0 {
				t.Errorf("stale recv data after the hand-offs: Transplanted=%d Unattributed=%d, want 0/0", tr, un)
			}
			if res.slow > 0 {
				msg := fmt.Sprintf("%d replies took over %s (the process's 10 ms ticker was at worst %s late)",
					res.slow, keepAliveSlowReply, res.canary.Round(time.Millisecond))
				if slowRepliesAreSuspect(res) {
					t.Errorf("%s: the process was not stalled, so the engine or the hand-off delayed them", msg)
				} else {
					t.Logf("%s: the process itself was stalled, not failed: celeris#892 (from #816 item 4)", msg)
				}
			}
			if res.errs != 0 {
				t.Errorf("clients saw %d errors (%s), want 0; attribution of the read timeouts: %s",
					res.errs, classes(res.byClass), tgt.attribute(res.timeouts))
			}
			got := tgt.adopted.Load()
			// The weak branch. The gate above leaves it reachable only
			// under async_without_cancel_flags (celeris#681 M1): a kernel
			// that predates 5.19 and so genuinely has no flags to find
			// gets no io_uring engine (celeris#682).
			if tc.async && !e.asyncCancelFlags {
				t.Logf("celeris681 no cancel flags: %d of %d promoted async conns handed off (promotions %d)",
					got, conns, e.Metrics().AsyncPromotedConns)
				if got != 0 {
					t.Errorf("%d of %d promoted async conns were handed off on a worker that cannot reap, want 0: "+
						"such a conn is never offered for the hand-off", got, conns)
				}
				return
			}
			if got != conns {
				t.Errorf("%d of %d busy conns were handed off, want all", got, conns)
			}
		})
	}
}

// asyncCancelProbeRecords describes what the engine's own probe said, from
// the records New wrote to buf: its class from "io_uring engine selected"
// (async_cancel_probe, logged on every kernel) and the reason from the
// "async cancel flags ..." record New adds where the flags were not found
// accepted. Reading New's records rather than calling the probe again keeps
// the message about the engine under test: the answer a second probe gave
// need not be the one this engine was built with (celeris#681 M1, N1).
func asyncCancelProbeRecords(t *testing.T, buf *lockedBuffer) string {
	t.Helper()
	var parts []string
	for _, r := range buf.records(t) {
		msg, _ := r["msg"].(string)
		switch {
		case msg == "io_uring engine selected":
			cls, _ := r["async_cancel_probe"].(string)
			parts = append(parts, fmt.Sprintf("New's probe class=%q", cls))
		case strings.HasPrefix(msg, "async cancel flags"):
			level, _ := r["level"].(string)
			reason, _ := r["reason"].(string)
			parts = append(parts, fmt.Sprintf("%s %q reason=%q", level, msg, reason))
		}
	}
	if len(parts) == 0 {
		return "no probe record (this subtest gave New no log buffer)"
	}
	return strings.Join(parts, "; ")
}

// asyncRouteHandler is transplantTestHandler with every route async, so each
// conn is promoted to its own dispatch goroutine on its first request.
type asyncRouteHandler struct{ transplantTestHandler }

func (asyncRouteHandler) RouteAsync(_, _ string) bool { return true }
func (asyncRouteHandler) HasAsyncRoutes() bool        { return true }

// TestStaleRecvDataCounted is the celeris#657 join as a test: every request a
// client lost is a stale recv CQE with data that the witness counted as
// Transplanted or Unattributed — and with the fix there are none of either.
func TestStaleRecvDataCounted(t *testing.T) {
	e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
	tgt := &servingTarget{}
	defer tgt.close()
	res := transplantUnderLoad(t, e, addr, 128, tgt, 300*time.Millisecond, 1500*time.Millisecond)
	w1 := int64(e.metrics.handoffLoss.staleRecvDataTransplanted.Load() +
		e.metrics.handoffLoss.staleRecvDataUnattributed.Load())
	if res.errs != w1 {
		t.Errorf("join broken: clients lost %d requests (%s) but the witness counted %d stale "+
			"data CQEs (Transplanted+Unattributed)", res.errs, classes(res.byClass), w1)
	}
	if res.errs != 0 || w1 != 0 {
		t.Errorf("lost requests = %d, stale data = %d, want 0 and 0", res.errs, w1)
	}
}

// TestHeldRecvIsReArmedWhenTheHandOffDoesNotHappen: a conn held for a
// hand-off that does not happen must be served on as if nothing had been
// attempted — (a) the target refuses every conn, (b) the drain stops while
// held responses are in flight, five times over. Clients see no error, no
// stale data appears, and no hold is left for the timeout sweep to rescue.
func TestHeldRecvIsReArmedWhenTheHandOffDoesNotHappen(t *testing.T) {
	check := func(t *testing.T, e *Engine, res loadResult) {
		t.Helper()
		if res.errs != 0 {
			t.Errorf("clients saw %d errors (%s), want 0", res.errs, classes(res.byClass))
		}
		if tr, un := e.metrics.handoffLoss.staleRecvDataTransplanted.Load(),
			e.metrics.handoffLoss.staleRecvDataUnattributed.Load(); tr != 0 || un != 0 {
			t.Errorf("stale recv data: Transplanted=%d Unattributed=%d, want 0/0", tr, un)
		}
		if n := e.metrics.handoffLoss.handoffInFlight.Load(); n != 0 {
			t.Errorf("TransplantHandoffInFlight = %d, want 0", n)
		}
		if n := metric(t, e, "TransplantHoldRescued"); n != 0 {
			t.Errorf("TransplantHoldRescued = %d, want 0: a held conn was stranded until the "+
				"timeout sweep found it", n)
		}
		for _, name := range []string{"TransplantDoubleClaim", "TransplantReapFailed"} {
			if n := metric(t, e, name); n != 0 {
				t.Errorf("%s = %d, want 0", name, n)
			}
		}
		if res.ok == 0 {
			t.Error("clients completed no requests")
		}
	}

	t.Run("target_refuses", func(t *testing.T) {
		e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
		tgt := &refusingTarget{}
		res := transplantUnderLoad(t, e, addr, 64, tgt, 200*time.Millisecond, 1200*time.Millisecond)
		if tgt.refused.Load() == 0 {
			t.Fatal("no hand-off was attempted; the test exercised nothing")
		}
		check(t, e, res)
	})

	t.Run("drain_stops_while_held", func(t *testing.T) {
		e, addr := startFDLEngine(t, transplantTestHandler{}, nil)
		tgt := &refusingTarget{}
		stop := make(chan struct{})
		wait := runKeepAliveLoad(t, addr, 64, stop)
		time.Sleep(200 * time.Millisecond)
		for i := 0; i < 5; i++ {
			e.StartTransplant(tgt)
			time.Sleep(100 * time.Millisecond)
			e.StopTransplant()
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(1200 * time.Millisecond) // a stranded conn gets no reply in here; it ends in wait() below as a rescued hold (check) or a read timeout
		close(stop)
		res := wait()
		t.Logf("celeris657 flaps=5 ok=%d errs=%d classes=[%s] refused=%d", res.ok, res.errs,
			classes(res.byClass), tgt.refused.Load())
		if tgt.refused.Load() == 0 {
			t.Fatal("no hand-off was attempted; the test exercised nothing")
		}
		check(t, e, res)
	})
}

// skipIfProvidedBuffers skips t when any of e's workers receives through a
// provided-buffer ring (CELERIS_IOURING_MULTISHOT_RECV=1 on a tier that
// registers one), which the caller's premise does not hold under: why says
// what is unreachable there. A test that would pass or fail for the wrong
// reason on that model says so instead (celeris#890 item 79, "With memlock unlimited and
// CELERIS_IOURING_MULTISHOT_RECV=1 ..."). An engine that
// reports no workers is a failure, not a skip: the check would never run.
func skipIfProvidedBuffers(t *testing.T, e *Engine, why string) {
	t.Helper()
	e.mu.Lock()
	workers := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	if len(workers) == 0 {
		t.Fatal("engine reports no workers")
	}
	for _, w := range workers {
		if w.bufRing != nil {
			t.Skipf("provided-buffer recv in use: %s", why)
		}
	}
}

// testHoldReleasedInTheWorkerLoop is TestHoldReleasedWhenDrainStops through
// the worker's own event loop (the inlined udSend dispatch site), made
// deterministic by a response large enough to keep its SEND in flight until
// the client reads it: the request arrives with a drain set (so the response
// is held), the drain stops, and only then does the SEND complete.
//
// The target refuses every conn. The warm-up response's SEND completion can
// reach the worker after the client has read it, and so after StartTransplant:
// the idle conn is then reaped and offered to the target before /big is sent.
// Refused, it is reclaimed onto io_uring and /big is served there as intended;
// an accepting target would own (and here close) the conn instead.
func testHoldReleasedInTheWorkerLoop(t *testing.T) {
	big := make([]byte, 3<<20)
	e, addr := startFDLEngine(t, bigBodyHandler{big: big}, nil)
	skipIfProvidedBuffers(t, e, "the /big response is not held on a worker with a buffer ring, so the test would exercise nothing")
	d := net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		_ = rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		})
		return serr
	}}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	get := func(path string, timeout time.Duration) (int, error) {
		if _, err := c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return 0, err
		}
		_ = c.SetReadDeadline(time.Now().Add(timeout))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return int(n), err
	}
	if _, err := get("/", 2*time.Second); err != nil {
		t.Fatalf("warm-up request: %v", err)
	}
	tgt := &fdlTarget{refuse: true}
	e.StartTransplant(tgt)
	if _, err := c.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write /big: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // served and held; its SEND waits on this client
	if n := metric(t, e, "TransplantHeld"); n == 0 {
		t.Fatal("the /big response was not held: the test exercised nothing")
	}
	e.StopTransplant()
	state := func() string {
		m := e.Metrics()
		return "adopted=" + strconv.FormatInt(tgt.adopted.Load(), 10) +
			" detached=" + strconv.FormatUint(m.TransplantDetached, 10) +
			" refused=" + strconv.FormatUint(m.TransplantHandoffRefused, 10) +
			" held=" + strconv.FormatUint(metric(t, e, "TransplantHeld"), 10) +
			" closes=" + strconv.FormatUint(m.CloseCount, 10)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read /big: %v (%s)", err, state())
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || int(n) != len(big) {
		t.Fatalf("read /big body: %d of %d bytes, err %v (%s)", n, len(big), err, state())
	}
	if _, err := get("/", 2*time.Second); err != nil {
		t.Fatalf("the request after a held response whose drain stopped got %v: the conn was "+
			"left with no recv armed", err)
	}
	if tgt.adopted.Load() != 0 {
		t.Fatalf("the refusing target adopted %d conns", tgt.adopted.Load())
	}
	if n := metric(t, e, "TransplantHoldRescued"); n != 0 {
		t.Fatalf("TransplantHoldRescued = %d, want 0: the release at the SEND completion did not run", n)
	}
}

type bigBodyHandler struct{ big []byte }

func (h bigBodyHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

// TestWorkerParksWithNothingPending (A5): a worker that parks must first
// submit what its last iteration queued. The last connection's close queues
// the cancel of its header timer in the same iteration that finds the worker
// idle; on the base it parks with that SQE unsubmitted, and it stays so until
// something wakes the worker.
func TestWorkerParksWithNothingPending(t *testing.T) {
	e, addr := startFDLEngine(t, transplantTestHandler{}, func(c *resource.Config) {
		c.ReadHeaderTimeout = 10 * time.Second // a kernel timer in flight per conn
	})
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	br := bufio.NewReader(c)
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	_ = c.Close()

	e.mu.Lock()
	workers := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); ; {
		parked := 0
		for _, w := range workers {
			if w.suspended.Load() {
				parked++
			}
		}
		if parked == len(workers) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d workers parked within 5s", parked, len(workers))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// suspended is stored after the park decision, so everything the worker
	// wrote before it — including the SQ ring's pending count — is visible
	// here, and a parked worker writes nothing.
	for i, w := range workers {
		if p := w.ring.Pending(); p != 0 {
			t.Errorf("worker %d parked with %d SQE(s) queued and never submitted", i, p)
		}
	}
}

// TestServingTargetAttributesTimeouts pins the classes servingTarget.attribute
// puts a client read timeout in, one conn of each: celeris#892 (from #816
// item 4).
func TestServingTargetAttributesTimeouts(t *testing.T) {
	tgt := &servingTarget{byPort: map[int]*adoptedConn{}}
	deadline := time.Now()
	add := func(port int, read, sent int64, sentAt time.Time) {
		ac := &adoptedConn{adoptedAt: time.Now()}
		ac.lastRead.Store(read)
		ac.lastSent.Store(sent)
		ac.lastSentAt.Store(sentAt.UnixNano())
		tgt.byPort[port] = ac
	}
	add(1, 6, 6, deadline.Add(-time.Second))  // the target answered request 7's predecessor; 7 never arrived
	add(2, 7, 6, deadline.Add(-time.Second))  // read 7, did not answer it
	add(3, 7, 7, deadline.Add(-time.Second))  // answered 7 before the client gave up: the client missed the response
	add(5, 7, 7, deadline.Add(2*time.Second)) // answered 7 after the client gave up: the target was late
	ts := []clientTimeout{
		{port: 1, sent: 7, deadline: deadline}, {port: 2, sent: 7, deadline: deadline},
		{port: 3, sent: 7, deadline: deadline}, {port: 4, sent: 7, deadline: deadline},
		{port: 5, sent: 7, deadline: deadline},
	}
	got := tgt.attribute(ts)
	for _, want := range []string{
		"never_adopted=1", "request_never_reached_target=1", "target_read_not_answered=1",
		"target_answered_client_missed_it=1", "target_answered_after_client_deadline=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("attribution %q lacks %q", got, want)
		}
	}
}

// TestSlowRepliesAreSuspect pins when a slow reply fails the hand-off test
// celeris#892 (from #816 item 4): only when the test process itself ran on
// time, as the canary shows, so the delay was the engine's. A stalled process
// (a host that starved it) is logged, not failed.
func TestSlowRepliesAreSuspect(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  loadResult
		want bool
	}{
		{"no_slow_reply", loadResult{canary: time.Millisecond}, false},
		{"slow_reply_on_a_quiet_process", loadResult{slow: 1, canary: 40 * time.Millisecond}, true},
		{"slow_reply_in_a_stalled_process", loadResult{slow: 1, canary: 2 * time.Second}, false},
	} {
		if got := slowRepliesAreSuspect(tc.res); got != tc.want {
			t.Errorf("%s: slowRepliesAreSuspect=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// keepAliveLoadAgainst serves each conn the function gets, and returns the
// address and the result of a one-client load against it.
func keepAliveLoadAgainst(t *testing.T, serve func(c net.Conn)) loadResult {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				serve(c)
			}()
		}
	}()
	stop := make(chan struct{})
	wait := runKeepAliveLoad(t, ln.Addr().String(), 1, stop)
	time.AfterFunc(1800*time.Millisecond, func() { close(stop) })
	return wait()
}

// TestKeepAliveLoadToleratesAStalledReply is the harness half of celeris#892
// (from #816 item 4): a reply that takes longer than the old flat 1 s read deadline
// (the host starved the process) is a slow reply, not an error, while a reply
// that never comes is still a read timeout, once keepAliveReadTimeout has
// passed. It failed with the 1 s deadline: errs=1 for the stalled reply.
func TestKeepAliveLoadToleratesAStalledReply(t *testing.T) {
	savedTimeout, savedSlow := keepAliveReadTimeout, keepAliveSlowReply
	t.Cleanup(func() { keepAliveReadTimeout, keepAliveSlowReply = savedTimeout, savedSlow })
	reply := func(c net.Conn) {
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\nok"))
	}
	t.Run("stalled_reply_is_slow_not_an_error", func(t *testing.T) {
		keepAliveReadTimeout, keepAliveSlowReply = 5*time.Second, time.Second
		first := true
		res := keepAliveLoadAgainst(t, func(c net.Conn) {
			br := bufio.NewReader(c)
			for {
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				if first {
					first = false
					time.Sleep(1300 * time.Millisecond) // longer than the old 1 s deadline
				}
				reply(c)
			}
		})
		if res.errs != 0 {
			t.Fatalf("a reply 1.3 s late was an error: errs=%d (%s)", res.errs, classes(res.byClass))
		}
		if res.slow < 1 { // under the load this models a second reply can be late too
			t.Errorf("slow=%d, want at least 1 (the stalled reply)", res.slow)
		}
	})
	t.Run("lost_request_still_times_out", func(t *testing.T) {
		keepAliveReadTimeout, keepAliveSlowReply = 1500*time.Millisecond, time.Second
		res := keepAliveLoadAgainst(t, func(c net.Conn) {
			br := bufio.NewReader(c)
			_, _ = http.ReadRequest(br)
			time.Sleep(3 * time.Second) // never answers within the deadline
		})
		if res.byClass["read_timeout"] != 1 || res.errs != 1 {
			t.Fatalf("a request that is never answered: errs=%d (%s), want one read_timeout", res.errs, classes(res.byClass))
		}
		if len(res.timeouts) != 1 {
			t.Errorf("timeouts recorded: %d, want 1", len(res.timeouts))
		}
	})
}
