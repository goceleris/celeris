// Package mono is the clock of the native engines' timeout stamps (celeris#1178,
// follow-up of celeris#1176 and #1177).
//
// # The problem
//
// The engines stamp their idle, read, write, header and detection timeouts and
// the timeout sweep from time.Now().UnixNano(), the wall clock, and so do
// internal/conn (HTTP/1) and the HTTP/2 stream code. A step of the wall clock
// (an NTP step, a VM clock sync, a resume from suspend) is then read as elapsed
// idle time: repeated forward steps cut working connections through the sweep,
// and backward steps stretch every deadline.
//
// # The clock
//
// [Now] is a monotonic clock ON THE WALL'S SCALE: the wall time taken once at
// process start plus the monotonic time since (base + time.Since(start)). It
// is about 1.8e18 like a Unix time in nanoseconds, so every "now - stamp"
// expression, every int64(time.Second) literal and every fixture that writes
// time.Now().UnixNano() into a stamp field stays valid while nothing steps.
// time.Since reads one clock where time.Now reads two, so it is also cheaper
// (about 17 ns against 34 ns on arm64, internal/mono's benchmarks).
//
// It is NOT drainclock.Now (small scale, shutdown deadlines) and NOT
// deferlinger.Now (small scale, the pause linger): a value of one is never
// comparable with a value of another, and the scan test of this package fails
// when one statement mixes them.
//
// # Route, then flip (the plan of celeris#1178)
//
// The stamps form ONE connected component: the sweep compares one `now` with
// the engines' stamps, with the HTTP/1 header deadline, with the WebSocket idle
// deadline and, through H2State.Sweep, with the HTTP/2 stream stamps; conn and
// the root package serve both engines. Moving part of it onto this clock leaves
// an edge between two clocks, and a mixed scale carries every step since the
// start of the process (a 75 s step backward cuts every new connection at its
// first sweep, for the rest of the process's life): worse than today, where a
// step only cuts the connections that were open during it. So:
//
//  1. The clock, its seam, the scan and the canary. [Now] first read the WALL
//     clock, so nothing changed a value.
//  2. Routing PRs (io_uring, epoll, conn and the root package, the HTTP/2 stream
//     code, StartTime) sent every wall stamp through this package. They never
//     changed a clock, so each was correct on its own.
//  3. The FLIP changes [Now] to the monotonic form (the constant flipped) in ONE
//     PR. It is the only change of a clock.
//
// # The scan test (scan_test.go)
//
// TestNoWallStamps walks the directories (not a package list) of the engines,
// internal/conn, internal/protocol, the root package and the WebSocket and SSE
// middleware, and matches SYNTACTICALLY, without type information: every call
// of .UnixNano(), .UnixMilli(), .UnixMicro() or .Unix(), every time.Unix(...),
// and the calls that strip a monotonic reading (.Round(0), .UTC(), .Local(),
// .In(...)). Each match, by (file, enclosing function, what) with a count,
// never by line, must be in the table stays with its reason (the Date header,
// the public SetWSIdleDeadline contract, one-second flood windows); the test
// also fails on a stale entry. During the routing steps a second table listed
// every site still to move and the test functions that wrote a wall reading into
// a stamp field; it is gone, so a routing PR reverted after the flip re-adds a
// match that is listed nowhere and CI goes red, and the flip cherry-picked
// without its routing PRs fails on the release branch. TestNoWallFixtures
// refuses a test function that writes a wall reading into a stamp field or hands
// one to a function that takes a stamp. A statement that mixes a drainclock or
// deferlinger reading with one of this package fails too.
//
// The scan has no types, so it cannot see a wall read written as time.Since(x) of
// a variable x that holds a time.Time without a monotonic reading (time.Unix,
// time.Date, Round(0)); time.Since(time.Date(...)) written out is seen. The skew
// canary is the backstop for that form: CI runs the engines' stepped-clock test
// under it, which fails ten to fourteen times on a loop-thread clock read that is
// not routed, and the canary subsets.
//
// # The skew canary (test builds only)
//
// The seam cannot step the real clock, so it cannot see a site that still reads
// the wall. The canary can: built with -tags celeris_monoskew and run with
// CELERIS_MONO_SKEW=3600s (or -3600s), [Now] reads one hour away from the wall
// (and the monotonic form starts one hour away from it). A site that was not
// routed then compares a wall stamp with a skewed one and fails at once,
// whatever the phase. The default build has no skew code: the constant is 0.
//
// # Public boundaries
//
// SetWSIdleDeadline takes a wall Unix time in nanoseconds (its contract does
// not change): the engines convert it with [FromWall], which keeps the time
// left. The sentinels 0 (unset) and 1 (expired) pass through unconverted, and
// no other input above 1 comes back as 0 or 1. Context.StartTime() returns
// [Time]: the true wall instant of a stamp, with a monotonic reading, so that
// time.Since of it ignores steps. [FromTime] stamps an existing time.Time (the
// HTTP/2 and std fallback of handler.go already has one) without a second clock
// read.
package mono

import (
	"math"
	"sync/atomic"
	"time"
)

// flipped selects the clock. true: [Now] is monotonic on the wall's scale (the
// flip of celeris#1178, after every stamp of the engines, conn, the root package
// and the HTTP/2 stream code was routed through this package; the scan test
// holds that: a wall read it does not list fails it). false would put [Now] back
// on the wall clock, value for value, which is what the routing steps ran on.
const flipped = true

// The lowest and highest value base can take: 2006 and 2116 in Unix
// nanoseconds. A host without a hardware clock can start a process at 1970 +
// its uptime; a base near 0 would put the stamps next to the sentinels the
// engines and conn read (0 is unset, 1 is expired): FromWall(w) of a past
// instant could come out at 1, and a deadline then reads as "unset".
const (
	minBase = int64(1) << 60
	maxBase = int64(1) << 62
)

// start anchors the clock. time.Now carries a monotonic reading, and time.Since
// between two readings that both carry one uses the monotonic clock alone
// (package time, "Monotonic Clocks"). It is never Rounded or re-read.
var start = time.Now()

// base is the wall time at start (clamped), plus the canary's skew.
var base = clampBase(start.UnixNano()) + skew

func clampBase(w int64) int64 { return min(max(w, minBase), maxBase) }

// Reading is one reading of both clocks, which is what a test replaces to step
// the wall without the monotonic clock (SetSeam).
type Reading struct {
	Wall int64 // time.Now().UnixNano()
	Mono int64 // nanoseconds on the monotonic clock since the package was initialised
}

// seam is a test seam: nil in production, set only by tests, which cannot
// step the process's wall clock (the machine's clock is shared). It costs one
// atomic load and a nil test per read (0.3 to 1 ns). It stays in the shipped
// function: a build tag would make the tested binary differ from the shipped
// one on the function under test.
var seam atomic.Pointer[func() Reading]

// cur is the ONE switch every function derives from: the clock's value for a
// reading r. flipped false: the wall (plus the canary's skew). true: the
// monotonic clock on the wall's scale.
func cur(r Reading, mono bool) int64 {
	if mono {
		return base + r.Mono
	}
	return r.Wall + skew
}

// curAt is cur for the instant t (one time.Now() already taken by the caller).
// With a seam installed the seam's reading is the clock; t stays the real
// instant (the wall time of a stamp is always real).
func curAt(t time.Time) int64 {
	if f := seam.Load(); f != nil {
		return cur((*f)(), flipped)
	}
	if flipped {
		return fromTimeMonotonic(t)
	}
	return t.UnixNano() + skew
}

// Now is the clock every stamp is taken and compared on: nanoseconds on the
// wall's scale: monotonic, a step of the wall clock does not move it. A stamp of it is never 0
// or 1, so 0 can still mean "not set" and 1 "expired". It is not comparable
// with a drainclock or deferlinger reading.
func Now() int64 {
	if f := seam.Load(); f != nil {
		return cur((*f)(), flipped)
	}
	if flipped {
		return nowMonotonic()
	}
	return time.Now().UnixNano() + skew
}

// nowMonotonic is the form of Now after the flip: one clock read.
func nowMonotonic() int64 { return base + int64(time.Since(start)) }

// FromWall puts a wall-clock Unix time in nanoseconds (a deadline a caller
// computed from time.Now(), as the public Context.SetWSIdleDeadline takes it)
// on Now's scale, keeping the time left to it: Now() + (w - the wall's now).
// The wall's now is the REAL wall, not a seam's, so a stepped seam moves Now
// and not the caller's reading, as in production, where the two are one
// clock.
//
// The sentinels pass through: w <= 1 (0 is "unset", 1 "expired") comes back
// as it is, and any other input comes back as at least 2, so that a deadline
// long past is never mistaken for a sentinel. The sum saturates.
func FromWall(w int64) int64 {
	if w <= 1 {
		return w
	}
	t := time.Now()
	d := w - t.UnixNano() // the time left, from the real wall
	c := curAt(t)
	v := c + d
	switch {
	case d > 0 && v < c: // overflow
		v = math.MaxInt64
	case v < 2:
		v = 2
	}
	return v
}

// Time puts a stamp of Now on the time.Time axis: the true wall time of the
// instant it was taken, with a monotonic reading, so that time.Since(Time(ns))
// is the monotonic age of the stamp and ignores steps. It is one time.Now()
// (the stamp's age on Now's scale is subtracted from it). Context.StartTime()
// is this: the instant a request's stamp was taken, as time.Unix(0, ns) would
// give it if the stamp were a wall time, but right however far the clock has
// moved from the wall since the process started.
func Time(ns int64) time.Time {
	t := time.Now()
	return t.Add(-time.Duration(curAt(t) - ns))
}

// Until is how long it is, on Now's clock, until the stamp ns: ns - Now(), as a
// duration (negative when it has passed). The waiters' timers are made of it. It
// replaces time.Until(time.Unix(0, ns)), which reads the WALL clock against a
// stamp of this one: the two differ by every step since the start of the process.
func Until(ns int64) time.Duration { return time.Duration(ns - Now()) }

// FromTime is the stamp of an instant t that the caller already has (a
// time.Now() it took for another reason, as handler.go does for a request
// that no engine stamped): no second clock read. t should carry a monotonic
// reading (a time.Now() does); one without (time.Unix, Round(0)) is placed by
// its wall time, as the clock stood at the start.
//
// With a seam installed the result is Now() minus the real age of t, so that
// it agrees with Now() whatever the seam steps.
func FromTime(t time.Time) int64 {
	if f := seam.Load(); f != nil {
		return cur((*f)(), flipped) - int64(time.Since(t))
	}
	if flipped {
		return fromTimeMonotonic(t)
	}
	return t.UnixNano() + skew
}

// fromTimeMonotonic is the form of FromTime (and of curAt) after the flip: no
// clock read, t's own monotonic reading against start's.
func fromTimeMonotonic(t time.Time) int64 { return base + int64(t.Sub(start)) }

// SetSeam makes the clock read f instead of the real clocks, until the returned
// function is called, which puts back what was set before, but only if this
// call's f is still the one installed. Tests only; the seam is process-wide,
// so a test that sets it runs no other engine meanwhile. The wall reading of
// the seam is not the clock (its Mono reading is): a test that steps Wall moves
// nothing that reads this package, and a wall reading that is not routed through it
// is what the scan and the skew canary find.
func SetSeam(f func() Reading) (restore func()) {
	var p *func() Reading
	if f != nil {
		p = &f
	}
	prev := seam.Swap(p)
	return func() { seam.CompareAndSwap(p, prev) }
}
