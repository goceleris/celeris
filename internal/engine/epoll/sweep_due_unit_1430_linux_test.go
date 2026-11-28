//go:build linux

package epoll

// celeris#1430, part S: the next-due computation as a table. checkTimeouts ends
// by storing, in nextDue, the earliest time at which a later walk has anything
// to do; sweepDue is what the tick and the gate ask. Each row builds a Loop
// literal with connections in one state, makes the walk, and compares nextDue
// with what the state calls for, exactly (the walk's own stamp is lastSweep).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/mono"
	"github.com/goceleris/celeris/internal/resource"
)

// dueConn1430 is an established HTTP/1 connection last read at last.
func dueConn1430(last int64, fd int) *connState {
	cs := acquireConnState(context.Background(), fd, 64, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.lastActivity = last
	return cs
}

func dueLoop1430(cfg resource.Config, conns ...*connState) *Loop {
	l := &Loop{cfg: cfg}
	for _, cs := range conns {
		cs.liveIdx = len(l.liveConns)
		l.liveConns = append(l.liveConns, cs)
	}
	return l
}

func TestNextDueIsTheEarliestOfWhatTheWalkSees1430(t *testing.T) {
	s0 := mono.Now()
	hour := int64(time.Hour)
	type want func(l *Loop, conns []*connState) int64
	atSweep := func(d time.Duration) want {
		return func(l *Loop, _ []*connState) int64 { return l.lastSweep + int64(d) }
	}
	exact := func(f func(conns []*connState) int64) want {
		return func(_ *Loop, c []*connState) int64 { return f(c) }
	}

	for _, tc := range []struct {
		name  string
		cfg   resource.Config
		build func() []*connState
		want  want
	}{
		{
			name: "nothing held: the horizon, a second",
			build: func() []*connState {
				return nil
			},
			want: atSweep(time.Second),
		},
		{
			name: "no timeout configured: the horizon",
			build: func() []*connState {
				return []*connState{dueConn1430(s0-int64(time.Hour), 3)}
			},
			want: atSweep(time.Second),
		},
		{
			name: "an idle connection far from its timeout: the horizon, a second",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				return []*connState{dueConn1430(s0-int64(time.Second), 3)}
			},
			want: atSweep(time.Second),
		},
		{
			name: "an idle connection near its timeout: last + IdleTimeout + 1 ns",
			cfg:  resource.Config{IdleTimeout: 2 * time.Second},
			build: func() []*connState {
				return []*connState{dueConn1430(s0-int64(1900*time.Millisecond), 3)}
			},
			want: exact(func(c []*connState) int64 { return c[0].lastActivity + int64(2*time.Second) + 1 }),
		},
		{
			name: "ReadTimeout shorter than IdleTimeout is the one",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second, ReadTimeout: 2 * time.Second},
			build: func() []*connState {
				return []*connState{dueConn1430(s0-int64(1900*time.Millisecond), 3)}
			},
			want: exact(func(c []*connState) int64 { return c[0].lastActivity + int64(2*time.Second) + 1 }),
		},
		{
			name: "a short timeout shortens the horizon to half of it",
			cfg:  resource.Config{ReadTimeout: 200 * time.Millisecond},
			build: func() []*connState {
				return []*connState{dueConn1430(s0+hour, 3)}
			},
			want: atSweep(100 * time.Millisecond),
		},
		{
			name: "a timeout under two ticks takes the horizon to nothing: the next tick walks",
			cfg:  resource.Config{IdleTimeout: 30 * time.Millisecond},
			build: func() []*connState {
				return []*connState{dueConn1430(s0+hour, 3)}
			},
			want: atSweep(0),
		},
		{
			name: "a timeout of two ticks keeps half of it, a tick",
			cfg:  resource.Config{IdleTimeout: 50 * time.Millisecond},
			build: func() []*connState {
				return []*connState{dueConn1430(s0+hour, 3)}
			},
			want: atSweep(sweepFloor),
		},
		{
			name: "ReadHeaderTimeout counts for the horizon too",
			cfg:  resource.Config{ReadHeaderTimeout: 400 * time.Millisecond},
			build: func() []*connState {
				return []*connState{dueConn1430(s0+hour, 3)}
			},
			want: atSweep(200 * time.Millisecond),
		},
		{
			name: "an armed header deadline: it + 1 ns",
			cfg:  resource.Config{ReadHeaderTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := dueConn1430(s0+hour, 3)
				cs.h1State.HeaderDeadlineNs.Store(s0 + int64(300*time.Millisecond))
				return []*connState{cs}
			},
			want: exact(func(c []*connState) int64 { return c[0].h1State.HeaderDeadlineNs.Load() + 1 }),
		},
		{
			name: "a connection whose protocol is not decided: its accept budget + 1 ns",
			cfg:  resource.Config{ReadHeaderTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := dueConn1430(s0+hour, 3)
				cs.detected = false
				cs.h1State = nil
				cs.detectDeadline = s0 + int64(400*time.Millisecond)
				return []*connState{cs}
			},
			want: exact(func(c []*connState) int64 { return c[0].detectDeadline + 1 }),
		},
		{
			name: "the earliest of several connections",
			cfg:  resource.Config{IdleTimeout: 2 * time.Second},
			build: func() []*connState {
				a := dueConn1430(s0-int64(1500*time.Millisecond), 3)
				b := dueConn1430(s0-int64(1900*time.Millisecond), 4)
				c := dueConn1430(s0-int64(500*time.Millisecond), 5)
				return []*connState{a, b, c}
			},
			want: exact(func(c []*connState) int64 { return c[1].lastActivity + int64(2*time.Second) + 1 }),
		},
		{
			name: "a hijack in flight: the next tick looks again",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := dueConn1430(s0, 3)
				cs.hijacked.Store(true)
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "a connection whose lock is held by a writer: nothing was read, the next tick looks again",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := acquireConnState(context.Background(), 3, 64, true)
				cs.protocol = engine.HTTP1
				cs.detected = true
				cs.h1State = conn.NewH1State()
				cs.lastActivity = s0
				cs.detachMu.Lock() // held for good: TryLock fails, and no dispatch goroutine is running
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "a closing drain: sampled at every tick",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second, WriteTimeout: time.Hour},
			build: func() []*connState {
				cs := dueConn1430(s0, 3)
				cs.closeSince, cs.closeSampled = s0, s0
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "a parked response: its clock is sampled at every tick",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second, WriteTimeout: time.Hour},
			build: func() []*connState {
				cs := dueConn1430(s0, 3)
				cs.writeSince = s0
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "an unflushed response on the dirty list: the next tick looks again",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := dueConn1430(s0, 3)
				cs.dirty = true
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "a detached connection: the next tick looks again",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				cs := dueConn1430(s0, 3)
				cs.h1State.Detached.Store(true)
				cs.h1State.IdleDeadlineNs.Store(s0 + hour)
				return []*connState{cs}
			},
			want: atSweep(0),
		},
		{
			name: "one connection that is not dated pulls a loop of idle ones to the next tick",
			cfg:  resource.Config{IdleTimeout: 10 * time.Second},
			build: func() []*connState {
				a := dueConn1430(s0, 3)
				b := dueConn1430(s0, 4)
				b.writeSince = s0
				return []*connState{a, b}
			},
			want: atSweep(0),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conns := tc.build()
			l := dueLoop1430(tc.cfg, conns...)
			l.checkTimeouts()
			exp := tc.want(l, conns)
			if l.nextDue != exp {
				t.Errorf("nextDue is %v after the walk, want %v (lastSweep + %v)", time.Duration(l.nextDue-l.lastSweep), time.Duration(exp-l.lastSweep), time.Duration(exp-l.lastSweep))
			}
			if over := time.Duration(l.nextDue - l.lastSweep); over > time.Second {
				t.Errorf("the next walk is %v after this one, want at most a second (the safety cap)", over)
			}
			if len(l.liveConns) != len(conns) {
				t.Errorf("the walk closed %d of %d connections: the row is built to close none", len(conns)-len(l.liveConns), len(conns))
			}
		})
	}
}

// TestSweepDue1430 is what the tick and the gate ask.
func TestSweepDue1430(t *testing.T) {
	now := mono.Now()
	idle := func() *Loop {
		l := dueLoop1430(resource.Config{}, dueConn1430(now, 3))
		l.lastSweep, l.nextDue = now, now+int64(time.Second)
		return l
	}
	for _, tc := range []struct {
		name string
		mut  func(l *Loop)
		at   int64
		want bool
	}{
		{"nothing due yet", func(*Loop) {}, now + int64(900*time.Millisecond), false},
		{"due at the stored time", func(*Loop) {}, now + int64(time.Second), true},
		{"past it", func(*Loop) {}, now + int64(2*time.Second), true},
		{"the clock stepped back", func(*Loop) {}, now - int64(time.Hour), true},
		{"no connection: nothing to skip", func(l *Loop) { l.liveConns = nil }, now + 1, true},
		{"an HTTP/2 connection keeps the floor", func(l *Loop) { l.h2Conns = []int{3} }, now + 1, true},
		{"a detached connection keeps the floor", func(l *Loop) { l.detachedCount = 1 }, now + 1, true},
		{"a dirty list keeps the floor", func(l *Loop) { l.dirtyHead = &connState{} }, now + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := idle()
			tc.mut(l)
			if got := l.sweepDue(tc.at); got != tc.want {
				t.Errorf("sweepDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLowerNextDue1430: a clock that starts between two walks brings the next
// one forward, never back.
func TestLowerNextDue1430(t *testing.T) {
	l := &Loop{nextDue: 100}
	l.lowerNextDue(150)
	if l.nextDue != 100 {
		t.Errorf("lowerNextDue(150) moved the next walk from 100 to %d", l.nextDue)
	}
	l.lowerNextDue(40)
	if l.nextDue != 40 {
		t.Errorf("lowerNextDue(40) left the next walk at %d", l.nextDue)
	}
}

// TestHooksBringTheNextWalkForward1430: markClosing starts a drain clock that is
// sampled from the next tick on, so it must not leave the next walk a second
// away.
func TestHooksBringTheNextWalkForward1430(t *testing.T) {
	l := dueLoop1430(resource.Config{WriteTimeout: time.Hour})
	cs := dueConn1430(mono.Now(), 3)
	l.nextDue = mono.Now() + int64(time.Second)
	l.markClosing(cs, 10)
	if l.nextDue > cs.closeSince {
		t.Errorf("markClosing left the next walk at %v after the drain began", time.Duration(l.nextDue-cs.closeSince))
	}
}

// TestSweepHorizon1430 is the safety cap as a function of the configuration: a
// second, half the shortest timeout a new deadline can be made of when that is
// shorter, and nothing (the next tick walks, as a loop did before the change)
// when that timeout is under two ticks.
func TestSweepHorizon1430(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name string
		cfg  resource.Config
		want time.Duration
	}{
		{"no timeout", resource.Config{}, time.Second},
		{"a disabled header timeout does not count", resource.Config{ReadHeaderTimeout: -1, IdleTimeout: 30 * time.Second}, time.Second},
		{"IdleTimeout 20 ms", resource.Config{IdleTimeout: 20 * ms}, 0},
		{"IdleTimeout 30 ms", resource.Config{IdleTimeout: 30 * ms}, 0},
		{"IdleTimeout 40 ms", resource.Config{IdleTimeout: 40 * ms}, 0},
		{"IdleTimeout 49 ms", resource.Config{IdleTimeout: 49 * ms}, 0},
		{"IdleTimeout 50 ms", resource.Config{IdleTimeout: 50 * ms}, 25 * ms},
		{"IdleTimeout 60 ms", resource.Config{IdleTimeout: 60 * ms}, 30 * ms},
		{"IdleTimeout 100 ms", resource.Config{IdleTimeout: 100 * ms}, 50 * ms},
		{"IdleTimeout 1.9 s", resource.Config{IdleTimeout: 1900 * ms}, 950 * ms},
		{"IdleTimeout 2 s", resource.Config{IdleTimeout: 2 * time.Second}, time.Second},
		{"IdleTimeout 30 s", resource.Config{IdleTimeout: 30 * time.Second}, time.Second},
		{"ReadTimeout 40 ms among long ones", resource.Config{ReadTimeout: 40 * ms, IdleTimeout: time.Minute, ReadHeaderTimeout: 10 * time.Second}, 0},
		{"ReadHeaderTimeout 40 ms among long ones", resource.Config{ReadHeaderTimeout: 40 * ms, IdleTimeout: time.Minute, ReadTimeout: time.Minute}, 0},
		{"WriteTimeout is not a timeout a deadline is made of", resource.Config{WriteTimeout: 10 * ms, IdleTimeout: 30 * time.Second}, time.Second},
	} {
		l := &Loop{cfg: tc.cfg}
		if got := time.Duration(l.sweepHorizon()); got != tc.want {
			t.Errorf("%s: sweepHorizon = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// lateness1430 runs the tick of one loop, on a clock the test moves, for a
// connection that arrives (or is read) at read after the walk at tick 0 and
// expires timeout after that, and returns the number of the first tick at which
// the connection is due (a walk at every tick, what the loop did before
// celeris#1430, meets it there) and the number of the first tick at which this
// loop walks it as due (the tick asks sweepDue, and walks when it says so). The
// ticks are sweepFloor apart, each late by jit(k). The loop holds one other
// connection, with nothing due for an hour, so that it is not one with nothing
// to walk; the connection under test is not on the loop at the walk at tick 0,
// which dated nothing of it (a connection read before that walk is dated by it
// and only brings the next walk nearer).
func lateness1430(t *testing.T, timeout time.Duration, read time.Duration, jit func(k int) time.Duration) (due, walked int, dueLate, walkedLate time.Duration) {
	t.Helper()
	m := mono.NewManual()
	defer mono.SetManual(m)()
	t0 := mono.Now()
	at := func(k int) int64 { return t0 + int64(time.Duration(k)*sweepFloor+jit(k)) }

	other := dueConn1430(at(0)+int64(time.Hour), 4)
	l := dueLoop1430(resource.Config{IdleTimeout: timeout}, other)
	m.Advance(time.Duration(at(0) - mono.Now()))
	l.checkTimeouts() // tick 0
	cs := dueConn1430(at(0)+int64(read), 3)
	cs.liveIdx = len(l.liveConns)
	l.liveConns = append(l.liveConns, cs)

	due, walked = -1, -1
	for k := 1; k < 400 && (due < 0 || walked < 0); k++ {
		m.Advance(time.Duration(at(k) - mono.Now()))
		now := mono.Now()
		isDue := l.timeoutsDue(time.Duration(now - cs.lastActivity))
		if isDue && due < 0 {
			due = k
		}
		if l.sweepDue(now) {
			if isDue {
				if walked < 0 {
					walked = k
				}
				continue // the walk would close the connection here
			}
			l.checkTimeouts()
		}
	}
	if due < 0 || walked < 0 {
		t.Fatalf("the connection was not due (%d) or not walked as due (%d) within 400 ticks", due, walked)
	}
	// How late each met it: the tick's time (what a walk at it reads) after the
	// deadline, which is the read plus the timeout.
	deadline := cs.lastActivity + int64(timeout)
	return due, walked, time.Duration(at(due) - deadline), time.Duration(at(walked) - deadline)
}

// TestShortTimeoutsAreMetNoLaterThanAWalkAtEveryTick1430: the walk now waits for
// a deadline, and the proof that it waits no longer than the tick a walk at
// every tick used is that its wait is under the shortest timeout by a tick or
// more (sweepHorizon). The test is that proof run: for each timeout, a read
// at several distances after a walk and the ticks late by several amounts, the
// first tick at which this loop walks a connection that is due is the first tick
// at which it is due. A timeout of 20 ms with a wait of one tick missed that tick
// when the next one came before the walk's own time plus the wait.
func TestShortTimeoutsAreMetNoLaterThanAWalkAtEveryTick1430(t *testing.T) {
	ms := time.Millisecond
	timeouts := []time.Duration{20 * ms, 24 * ms, 25 * ms, 30 * ms, 40 * ms, 49 * ms, 50 * ms, 51 * ms, 60 * ms, 75 * ms, 100 * ms, 130 * ms, 1000 * ms, 2300 * ms}
	reads := []time.Duration{-3 * ms, 0, ms, 7 * ms, 12 * ms, 24 * ms}
	jitters := map[string]func(k int) time.Duration{
		"ragged": func(k int) time.Duration {
			return time.Duration((k*7919)%11) * ms
		},
		"late, then early": func(k int) time.Duration { return []time.Duration{9 * ms, 0}[k%2] },
		"early, then late": func(k int) time.Duration { return []time.Duration{0, 9 * ms}[k%2] },
	}
	// The walk at tick 0 late by d, every later tick on time (and the other way
	// round): the next tick then comes d before, or after, the walk's own time
	// plus a tick, for every d in a millisecond's steps.
	for d := time.Duration(0); d <= 12*ms; d += ms {
		jitters[fmt.Sprintf("the walk %v late, the next ticks on time", d)] = func(k int) time.Duration {
			if k == 0 {
				return d
			}
			return 0
		}
		jitters[fmt.Sprintf("the next ticks %v late, the walk on time", d)] = func(k int) time.Duration {
			if k == 0 {
				return 0
			}
			return d
		}
	}
	for _, T := range timeouts {
		var worstMain, worstHere time.Duration
		for name, jit := range jitters {
			for _, read := range reads {
				due, walked, mainLate, hereLate := lateness1430(t, T, read, jit)
				if walked != due {
					t.Errorf("timeout %v, read %v after the walk, ticks %s: due at tick %d, walked as due at tick %d (a walk at every tick meets it at the first)",
						T, read, name, due, walked)
				}
				worstMain, worstHere = max(worstMain, mainLate), max(worstHere, hereLate)
			}
		}
		// The table of the pull request: the worst lateness over the grid.
		t.Logf("celeris1430 LATENESS timeout %v: a walk at every tick %v, this loop %v", T, worstMain, worstHere)
	}
}
