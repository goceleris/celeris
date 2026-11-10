//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#1225 (found by the review of #1224): a connection that has begun a
// request head and not finished it, a fragment or a bare CRLF, when Shutdown
// begins. The tests characterise what the engines do today; a change to one of
// them fails here and has to change the Shutdown godoc with it (its text is
// pinned by shutdown_doc_1225_test.go). Measured on arm64, in docker with 2
// CPUs, a 7.0 kernel, a 2 s budget:
//
//	epoll, io_uring, adaptive (on epoll)
//	    The connection is a request in progress, whether it had served a request
//	    before (ka-*) or is new (new-*): Shutdown does not return before ctx's
//	    deadline, returns there (ctx's error; nil in a few runs under load, when
//	    the engine's own drain ends at the same moment and wins the race with
//	    ctx in Server.Shutdown's wait for Listen), and the connection gets its
//	    FIN right after (2.00 to 2.01 s). The idle connections beside it are closed at
//	    once or, on the loop (epoll) or the engine (io_uring) that has it, at
//	    about 250 ms (the gate's floor). Completing the head is answered, with
//	    Connection: close, and Shutdown returns (about 10 ms after it).
//	    Shutdown returns nil, before the budget, when a limit of the connection
//	    ends the wait: ReadHeaderTimeout, counted from the head's first byte on
//	    a keep-alive conn and from the engine's accept on a new one (trickling
//	    does not extend it), or WriteTimeout, from the start of the wait.
//	std (net/http)
//	    A keep-alive connection that has begun a head is StateIdle until
//	    readRequest returns: it is closed with the idle ones, without an answer,
//	    and Shutdown returns nil at once (1 ms). A new connection that has begun
//	    its first head is StateNew, not idle until it is 5 s old: Shutdown waits
//	    for it to the budget and returns ctx's error, and the connection is left
//	    open (still open 4 s later). ReadHeaderTimeout ends it with Shutdown
//	    returning nil. A bare CRLF as the first bytes of a new connection is
//	    answered 400 before any shutdown, so there is no std cell of it.
//
// What the cells judge, so that a starved machine cannot fail them: the
// verdict of a hold is Shutdown's own return, which no delay can fake (a return
// before the budget ended, or with an error that is not ctx's, is a failure
// whatever the machine does), and the
// connection's FIN, which a late test goroutine can only observe later. An
// outcome that only a slow machine, and a regression, can both produce (an idle
// connection that was not closed promptly, a Shutdown that ran on past a limit)
// is not a verdict: the attempt is run again on a fresh rig, and the cell fails
// when none of conclusiveAttempts1225 attempts says anything. The mutants of
// this contract (an engine that closes the connection at its first pass) fail
// every attempt, as a failure or as inconclusive ones.

const (
	// budget1225 is the ctx deadline the cells give Shutdown. The defaults
	// (ReadHeaderTimeout 10 s, WriteTimeout 60 s) are both longer, so only ctx
	// can end a hold in the cells that leave them alone.
	budget1225 = 2 * time.Second

	// promptFrac1225 of the budget is what "promptly" means for an idle
	// connection beside the held one: well after the gate's floor (250 ms), and
	// well before the budget.
	promptFrac1225 = 0.4

	conclusiveAttempts1225 = 8

	idleConns1225 = 24

	// idleFloor1225 is a time the last idle conn's FIN must have reached when one
	// is on the holder's loop: well under the gate's 250 ms floor, measured at 251 to 266 ms.
	idleFloor1225 = 150 * time.Millisecond
)

type partialShape1225 struct {
	name    string
	prior   bool          // a keep-alive connection: it has served a request when it begins the head
	payload string        // what it sends before Shutdown begins
	rest    string        // what completes the head
	gap     time.Duration // a pause between the dial and the first byte, on a new conn
}

const headRest1225 = "P/1.1\r\nHost: x\r\n\r\n"

var (
	kaCRLF1225  = partialShape1225{"ka-crlf", true, "\r\n", get1208, 0}
	kaFrag1225  = partialShape1225{"ka-frag", true, "GET /ping HTT", headRest1225, 0}
	newCRLF1225 = partialShape1225{"new-crlf", false, "\r\n", get1208, 0}
	newFrag1225 = partialShape1225{"new-frag", false, "GET /ping HTT", headRest1225, 0}
)

// shapes1225 is what each engine is given: std has no cell of a bare CRLF on a
// new connection (it answers 400 at once, before any shutdown).
func shapes1225(native bool) []partialShape1225 {
	if native {
		return []partialShape1225{kaCRLF1225, kaFrag1225, newCRLF1225, newFrag1225}
	}
	return []partialShape1225{kaCRLF1225, kaFrag1225, newFrag1225}
}

// end1225 is how a read of a connection ended: ReadAll's bytes (an answer, when
// there are any), its error (nil: a FIN) and when, since begin.
type end1225 struct {
	data []byte
	err  error
	at   time.Duration
}

// closed reports a close with no answer: a FIN, or a reset for the holder.
func (e end1225) closed(reset bool) bool {
	return len(e.data) == 0 && (e.err == nil || (reset && errors.Is(e.err, syscall.ECONNRESET)))
}

func (e end1225) String() string {
	return fmt.Sprintf("data=%q err=%v at %v", e.data, e.err, e.at.Round(time.Millisecond))
}

// endOf1225 reads c to its end, or to the deadline, from a goroutine.
func endOf1225(c net.Conn, begin time.Time, deadline time.Time) <-chan end1225 {
	ch := make(chan end1225, 1)
	go func() {
		_ = c.SetReadDeadline(deadline)
		b, err := io.ReadAll(c)
		ch <- end1225{b, err, time.Since(begin)}
	}()
	return ch
}

type shut1225 struct {
	err error
	at  time.Duration // since begin
}

// shutdownBy1225 calls Shutdown with a ctx whose deadline is begin+budget, from
// a goroutine, and reports its return. begin is taken before the call, on the
// monotonic clock, so that a return reported at less than budget is a return
// before the deadline whatever the machine does.
func (r *rig1208) shutdownBy1225(begin time.Time, budget time.Duration) <-chan shut1225 {
	ctx, cancel := context.WithDeadline(context.Background(), begin.Add(budget))
	ch := make(chan shut1225, 1)
	go func() {
		defer cancel()
		err := r.s.Shutdown(ctx)
		ch <- shut1225{err, time.Since(begin)}
	}()
	return ch
}

type partialRig1225 struct {
	r      *rig1208
	idles  []net.Conn
	holder net.Conn
	opened time.Time // when holder was dialed
	sent   time.Time // when its payload was written
	stop   func()    // ends the holder's trickle, if any
}

// setupPartial1225 starts a server with idle keep-alive connections that have
// each been served, and a holder that has begun a head the server has read:
// the holder is dialed last, so that on std a new connection is as young as it
// can be.
func setupPartial1225(t *testing.T, cfg celeris.Config, sh partialShape1225, trickle bool) *partialRig1225 {
	t.Helper()
	cfg.DisableDeferAccept = true
	r := startRig1208(t, cfg, routes1208)
	base := settledEngineMetrics1104Server(r.s)
	p := &partialRig1225{r: r, stop: func() {}}
	for range idleConns1225 {
		c := r.dial()
		if o := send1208(c, get1208, 0); !o.answered() || o.close {
			t.Fatalf("an idle keep-alive conn before the shutdown: %v", o)
		}
		p.idles = append(p.idles, c)
	}
	p.holder, p.opened = r.dial(), time.Now()
	if sh.prior {
		if o := send1208(p.holder, get1208, 0); !o.answered() || o.close {
			t.Fatalf("the holder's first request: %v", o)
		}
	}
	r.waitConns(base, idleConns1225+1)
	time.Sleep(sh.gap)
	if _, err := io.WriteString(p.holder, sh.payload); err != nil {
		t.Fatal(err)
	}
	p.sent = time.Now()
	// What the engine has read, not what is in the kernel: the head is begun
	// whenever the loop gets to it.
	waitServerReadAll1166(t, r.addr, append([]net.Conn{p.holder}, p.idles...)...)
	if trickle {
		stop := make(chan struct{})
		var once sync.Once
		p.stop = func() { once.Do(func() { close(stop) }) }
		t.Cleanup(p.stop)
		go func() {
			// One byte per 150 ms into a header value (the head is
			// "GET /ping HTTP/1.1\r\nX-A: ..."): progress that never completes it.
			for {
				select {
				case <-stop:
					return
				case <-time.After(150 * time.Millisecond):
					if _, err := io.WriteString(p.holder, "a"); err != nil {
						return
					}
				}
			}
		}()
	}
	return p
}

// idleEnds reads each idle connection to its end, from begin.
func (p *partialRig1225) idleEnds(begin time.Time, within time.Duration) []<-chan end1225 {
	var chs []<-chan end1225
	for _, c := range p.idles {
		chs = append(chs, endOf1225(c, begin, begin.Add(within)))
	}
	return chs
}

// idlePrompt1225 reports whether every idle connection ended with a FIN before
// promptFrac1225 of the budget, and describes the first that did not; last is
// when the latest of them ended.
func idlePrompt1225(chs []<-chan end1225, budget time.Duration) (ok bool, why string, last time.Duration) {
	ok = true
	for i, ch := range chs {
		e := <-ch
		last = max(last, e.at)
		if ok && (!e.closed(false) || float64(e.at) >= promptFrac1225*float64(budget)) {
			ok, why = false, fmt.Sprintf("idle conn %d: %v, want a FIN before %v", i, e, time.Duration(promptFrac1225*float64(budget)))
		}
	}
	return ok, why, last
}

// finish closes the clients and waits for Start to return, which it does once
// Shutdown has, whatever Shutdown returned.
func (p *partialRig1225) finish(t *testing.T, conns ...net.Conn) {
	t.Helper()
	p.stop()
	for _, c := range append(conns, p.holder) {
		_ = c.Close()
	}
	for _, c := range p.idles {
		_ = c.Close()
	}
	select {
	case <-p.r.done:
	case <-time.After(10 * time.Second):
		t.Error("the server did not stop")
	}
}

// waitShut1225 waits for the Shutdown that shutdownBy1225 began.
func waitShut1225(t *testing.T, ch <-chan shut1225, budget time.Duration) shut1225 {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(budget + 15*time.Second):
		t.Fatalf("Shutdown did not return %v after its budget ended", 15*time.Second)
		return shut1225{}
	}
}

// conclusiveRuns1225 is floorRuns1244's loop for these cells: attempt runs on a
// fresh rig until it passes or fails with evidence; an inconclusive attempt is
// logged and run again, and the cell fails when none of conclusiveAttempts1225
// said anything.
func conclusiveRuns1225(t *testing.T, what string, attempt func() verdict1244) {
	t.Helper()
	var last string
	for i := 1; i <= conclusiveAttempts1225; i++ {
		v := attempt()
		switch {
		case v.ok:
			if i > 1 {
				t.Logf("%s: decided on attempt %d of %d", what, i, conclusiveAttempts1225)
			}
			return
		case v.fail != "":
			t.Errorf("%s: %s", what, v.fail)
			return
		}
		last = v.inconclusive
		t.Logf("%s: attempt %d of %d inconclusive (celeris#1225): %s", what, i, conclusiveAttempts1225, last)
		if t.Failed() {
			return
		}
	}
	t.Errorf("%s: none of %d attempts said anything about the contract; the last: %s", what, conclusiveAttempts1225, last)
}

func failf1225(format string, a ...any) verdict1244 {
	return verdict1244{fail: fmt.Sprintf(format, a...)}
}

func inconclusivef1225(format string, a ...any) verdict1244 {
	return verdict1244{inconclusive: fmt.Sprintf(format, a...)}
}

// TestShutdownPartialHeadHeld_1225 pins what Shutdown does to a connection that
// has begun a head and not finished it, per engine: the native engines hold
// Shutdown to ctx's deadline, and then close the connection; std closes a
// keep-alive connection in that state at once, with the idle ones, and holds a new
// one (StateNew) to the deadline, leaving it open. The idle connections beside
// it are closed promptly everywhere.
func TestShutdownPartialHeadHeld_1225(t *testing.T) {
	for _, ec := range engines1208 {
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()
			for _, sh := range shapes1225(ec.native) {
				t.Run(sh.name, func(t *testing.T) {
					held := ec.native || !sh.prior // std: only a new conn is held
					conclusiveRuns1225(t, ec.name+"/"+sh.name, func() verdict1244 { return heldAttempt1225(t, ec.name, ec.eng, ec.native, sh, held) })
				})
			}
		})
	}
}

func heldAttempt1225(t *testing.T, name string, eng celeris.EngineType, native bool, sh partialShape1225, held bool) verdict1244 {
	p := setupPartial1225(t, celeris.Config{Engine: eng}, sh, false)
	begin := time.Now()
	shut := p.r.shutdownBy1225(begin, budget1225)
	idle := p.idleEnds(begin, budget1225+5*time.Second)
	// A native holder is closed right after the budget; std leaves a held one open:
	// 400 ms is the look at that.
	holderUntil := begin.Add(budget1225 + 5*time.Second)
	if !native && held {
		holderUntil = begin.Add(budget1225 + 400*time.Millisecond)
	}
	holder := endOf1225(p.holder, begin, holderUntil)
	res := waitShut1225(t, shut, budget1225)
	he := <-holder
	prompt, why, lastIdle := idlePrompt1225(idle, budget1225)
	p.finish(t)
	ctxErr := errors.Is(res.err, context.DeadlineExceeded)

	switch {
	case !held: // std, a keep-alive conn: StateIdle until the head is read in full
		if ctxErr {
			if prompt {
				return failf1225("std: Shutdown ran to the budget (%v after %v) with a keep-alive conn that began a head, whose idle neighbours closed promptly: want it closed with the idle ones", res.err, res.at.Round(time.Millisecond))
			}
			return inconclusivef1225("std: Shutdown ran to the budget and its idle conns did not close promptly (%s)", why)
		}
		if res.err != nil {
			return failf1225("std: Shutdown returned %v, want nil", res.err)
		}
		if !he.closed(true) {
			return failf1225("std: the keep-alive conn that began a head: %v, want it closed without an answer", he)
		}
		if !prompt {
			return inconclusivef1225("std: %s", why)
		}
		return verdict1244{ok: true}

	case !native: // std, a new conn: StateNew, held to the budget, left open
		if res.err == nil {
			if res.at < budget1225 && time.Since(p.opened) < 4*time.Second {
				return failf1225("std: Shutdown returned nil after %v with a new conn that began its first head, %v old: want it held to the budget", res.at.Round(time.Millisecond), time.Since(p.opened).Round(time.Millisecond))
			}
			return inconclusivef1225("std: Shutdown returned nil after %v, the conn %v old: the conn is old enough for net/http to count it idle", res.at.Round(time.Millisecond), time.Since(p.opened).Round(time.Millisecond))
		}
		if !ctxErr {
			return failf1225("std: Shutdown returned %v, want ctx's error", res.err)
		}
		if !isTimeout1208(he.err) || len(he.data) != 0 {
			if time.Since(p.opened) >= 8*time.Second {
				return inconclusivef1225("std: the conn ended (%v) when it was %v old, near ReadHeaderTimeout", he, time.Since(p.opened).Round(time.Millisecond))
			}
			return failf1225("std: the new conn that began its first head: %v, want it still open 400 ms after Shutdown returned ctx's error (net/http leaves it)", he)
		}
		if !prompt {
			return inconclusivef1225("std: %s", why)
		}
		return verdict1244{ok: true}
	}

	// epoll, io_uring, adaptive: held to the budget, then closed.
	// At the deadline Shutdown returns ctx's error, or nil when the drain's own
	// end at the same moment wins the race with ctx (server.go, waitListen: seen
	// under load): either is a hold to the budget; a return before it is not.
	if res.at < budget1225 || (res.err != nil && !ctxErr) {
		return failf1225("%s: Shutdown returned %v after %v with a conn that began a head, whose bytes the engine had read: want it held to the %v budget", name, res.err, res.at.Round(time.Millisecond), budget1225)
	}
	if he.at < budget1225 {
		return failf1225("%s: the conn that began a head ended (%v) before the %v budget did: want it held to it", name, he, budget1225)
	}
	if !he.closed(true) {
		return failf1225("%s: the conn that began a head: %v, want a FIN (or a reset) without an answer once the budget ended", name, he)
	}
	if res.at > budget1225+3*time.Second {
		return inconclusivef1225("%s: Shutdown returned after %v, %v past its budget", name, res.at.Round(time.Millisecond), (res.at - budget1225).Round(time.Millisecond))
	}
	if !prompt {
		return inconclusivef1225("%s: %s", name, why)
	}
	// The rig has two loops and the kernel spreads the idle conns over them: those
	// on the other loop are closed at once, those on the holder's loop at the gate's
	// floor (250 ms). With idleConns1225 of them one is on the holder's loop with
	// p = 1 - 2^-idleConns1225; an attempt whose last FIN came before the floor
	// either had none there or closed them all at once, and says nothing of the
	// gate that holds a begun head: it is run again, and fails when no attempt has
	// one (an engine that closes at once, or never, fails every attempt).
	if lastIdle < idleFloor1225 {
		return inconclusivef1225("%s: the last of %d idle conns had its FIN at %v, before %v: none was on the holder's loop, or the gate closed them all at once", name, idleConns1225, lastIdle.Round(time.Millisecond), idleFloor1225)
	}
	return verdict1244{ok: true}
}

// TestShutdownPartialHeadCompleted_1225 pins the other end of the wait on the
// native engines: the head that completes during it is a request in progress, read
// and answered with Connection: close, and Shutdown returns, nil, long before the
// budget. (std closes such a connection at once, or drops the request, see the
// cells above and #1208.)
func TestShutdownPartialHeadCompleted_1225(t *testing.T) {
	for _, ec := range engines1208 {
		if !ec.native {
			continue
		}
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()
			for _, sh := range shapes1225(true) {
				t.Run(sh.name, func(t *testing.T) {
					conclusiveRuns1225(t, ec.name+"/"+sh.name, func() verdict1244 { return completedAttempt1225(t, ec.eng, sh) })
				})
			}
		})
	}
}

func completedAttempt1225(t *testing.T, eng celeris.EngineType, sh partialShape1225) verdict1244 {
	const budget = 4 * time.Second
	p := setupPartial1225(t, celeris.Config{Engine: eng}, sh, false)
	begin := time.Now()
	shut := p.r.shutdownBy1225(begin, budget)
	idle := p.idleEnds(begin, budget)
	// Every idle conn's FIN says the shutdown has begun on every loop, and one of
	// them on the holder's loop (the last, at the gate's floor) that its gate is up.
	prompt, why, lastIdle := idlePrompt1225(idle, budget)
	if lastIdle < idleFloor1225 && prompt {
		why = fmt.Sprintf("the last idle conn had its FIN at %v: none was on the holder's loop, where the gate closes them at its floor", lastIdle.Round(time.Millisecond))
		prompt = false
	}
	if !prompt {
		// The head is completed into a shutdown that may not have begun on its
		// loop: not the premise. Let it run out, and run again.
		r := waitShut1225(t, shut, budget)
		p.finish(t)
		return inconclusivef1225("%v: %s (Shutdown returned %v after %v)", eng, why, r.err, r.at.Round(time.Millisecond))
	}
	time.Sleep(100 * time.Millisecond)
	_ = p.holder.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, werr := io.WriteString(p.holder, sh.rest)
	written := time.Since(begin)
	_ = p.holder.SetReadDeadline(begin.Add(budget + 5*time.Second))
	o := classify1208(bufio.NewReader(p.holder), werr)
	res := waitShut1225(t, shut, budget)
	p.finish(t)

	if (res.at >= budget || errors.Is(res.err, context.DeadlineExceeded)) && written > budget/2 {
		return inconclusivef1225("%v: the head was completed %v into a %v budget: a loaded machine may have used it up (Shutdown: %v)", eng, written.Round(time.Millisecond), budget, res.err)
	}
	if !o.answered() || !o.close || o.body != "ok" {
		return failf1225("%v: the head completed %v into the shutdown: %v, want it answered with Connection: close", eng, written.Round(time.Millisecond), o)
	}
	if res.err != nil || res.at >= budget {
		return failf1225("%v: Shutdown returned %v after %v, the head completed and answered %v into it: want nil, as soon as the request was answered", eng, res.err, res.at.Round(time.Millisecond), written.Round(time.Millisecond))
	}
	return verdict1244{ok: true}
}

// TestShutdownPartialHeadLimits_1225 pins the two limits of the connection that
// end the wait before ctx's deadline, and so make Shutdown return nil: on every
// engine ReadHeaderTimeout, counted from the head's first byte and not extended
// by a head trickled in a byte at a time; on the native engines WriteTimeout,
// counted from the start of the wait.
func TestShutdownPartialHeadLimits_1225(t *testing.T) {
	for _, ec := range engines1208 {
		t.Run(ec.name, func(t *testing.T) {
			t.Parallel()
			// A trickling head on a new connection (on std a keep-alive one is
			// closed at once): the bytes keep coming, the header deadline does not move.
			t.Run("ReadHeaderTimeout/new-trickle", func(t *testing.T) {
				conclusiveRuns1225(t, ec.name+"/new-trickle", func() verdict1244 { return rhtAttempt1225(t, ec.eng, newFrag1225) })
			})
			if ec.native {
				t.Run("ReadHeaderTimeout/ka-trickle", func(t *testing.T) {
					conclusiveRuns1225(t, ec.name+"/ka-trickle", func() verdict1244 { return rhtAttempt1225(t, ec.eng, kaFrag1225) })
				})
				t.Run("WriteTimeout/ka-crlf", func(t *testing.T) {
					conclusiveRuns1225(t, ec.name+"/WriteTimeout", func() verdict1244 { return wtAttempt1225(t, ec.eng) })
				})
			}
		})
	}
}

const limitsBudget1225 = 4 * time.Second

func rhtAttempt1225(t *testing.T, eng celeris.EngineType, sh partialShape1225) verdict1244 {
	// The head's header line is open ("X-A: ") and gets a byte every 150 ms.
	sh.payload = "GET /ping HTTP/1.1\r\nX-A: "
	if !sh.prior {
		// A new conn's header deadline counts from its accept, not from its first
		// byte (DisableDeferAccept here): 300 ms between the two is the probe of
		// the review of #1253, which a verdict from the first byte failed on every
		// native engine.
		sh.gap = 300 * time.Millisecond
	}
	const rht = time.Second
	p := setupPartial1225(t, celeris.Config{Engine: eng, ReadHeaderTimeout: rht}, sh, true)
	begin := time.Now()
	shut := p.r.shutdownBy1225(begin, limitsBudget1225)
	holder := endOf1225(p.holder, begin, begin.Add(limitsBudget1225+5*time.Second))
	res := waitShut1225(t, shut, limitsBudget1225)
	he := <-holder
	p.finish(t)
	if res.err != nil || res.at >= limitsBudget1225 {
		// Shutdown ran to the budget (4 s) past a ReadHeaderTimeout of 1 s, counted
		// from a head that began before it: a regression, or a machine that kept
		// the engine from its timer for 3 s.
		return inconclusivef1225("%v: Shutdown returned %v after %v, ReadHeaderTimeout 1 s (the head was %v old when it began): want nil once the header deadline cut the conn", eng, res.err, res.at.Round(time.Millisecond), (time.Since(p.opened) - time.Since(begin)).Round(time.Millisecond))
	}
	// ReadHeaderTimeout counts, for a keep-alive conn, from the head's first byte,
	// which the engine read after it was written; for a new conn, from the
	// engine's accept of it (internal/conn/h1.go: the first request as a whole),
	// which is the dial here (the rig sets DisableDeferAccept; by default the
	// kernel puts it at the first byte), and net/http's, from when it began to
	// read the conn, also after the dial. Shutdown cannot return, nil, before that
	// plus the timeout, so a return before it is a Shutdown that did not wait for
	// the head.
	from := p.sent
	if !sh.prior {
		from = p.opened
	}
	if at := begin.Add(res.at); at.Before(from.Add(rht - 100*time.Millisecond)) {
		return failf1225("%v: Shutdown returned nil %v after the head began, %v before ReadHeaderTimeout %v: want it held until the header deadline cut the conn", eng, at.Sub(from).Round(time.Millisecond), (rht - at.Sub(from)).Round(time.Millisecond), rht)
	}
	if !he.closed(true) {
		return failf1225("%v: the conn whose head ReadHeaderTimeout cut: %v, want it closed without an answer", eng, he)
	}
	return verdict1244{ok: true}
}

func wtAttempt1225(t *testing.T, eng celeris.EngineType) verdict1244 {
	const wt = time.Second
	p := setupPartial1225(t, celeris.Config{Engine: eng, WriteTimeout: wt}, kaCRLF1225, false)
	begin := time.Now()
	shut := p.r.shutdownBy1225(begin, limitsBudget1225)
	holder := endOf1225(p.holder, begin, begin.Add(limitsBudget1225+5*time.Second))
	res := waitShut1225(t, shut, limitsBudget1225)
	he := <-holder
	p.finish(t)
	if res.err != nil || res.at >= limitsBudget1225 {
		return inconclusivef1225("%v: Shutdown returned %v after %v, WriteTimeout %v: want nil once the cap on the wait ended it", eng, res.err, res.at.Round(time.Millisecond), wt)
	}
	// The wait began after begin: it cannot end before wt on the engine's clock,
	// which starts later than this one.
	if res.at < wt*9/10 {
		return failf1225("%v: Shutdown returned after %v, before WriteTimeout %v from the start of the wait, with a conn that began a head", eng, res.at.Round(time.Millisecond), wt)
	}
	if !he.closed(true) {
		return failf1225("%v: the conn that began a head: %v, want it closed without an answer", eng, he)
	}
	return verdict1244{ok: true}
}
