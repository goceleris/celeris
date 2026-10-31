package mono

import (
	"math"
	"strings"
	"testing"
	"time"
)

// tol is the slack of the comparisons with the real clocks (a loaded CI runner).
const tol = int64(250 * time.Millisecond)

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Until the flip Now is the wall clock (plus the canary's skew, 0 in a normal
// build): value-neutral. After it, it is on the wall's scale.
func TestNowIsOnTheWallsScale(t *testing.T) {
	if d := Now() - (time.Now().UnixNano() + skew); abs(d) > tol {
		t.Fatalf("Now is %v away from the wall clock (skew %v)", time.Duration(d), time.Duration(skew))
	}
	if n := nowMonotonic(); abs(n-(time.Now().UnixNano()+skew)) > tol {
		t.Fatalf("the monotonic form is %v from the wall clock", time.Duration(n-time.Now().UnixNano()))
	}
}

func TestMonotonicFormNeverGoesBack(t *testing.T) {
	prev := nowMonotonic()
	for range 2_000_000 {
		c := nowMonotonic()
		if c < prev {
			t.Fatalf("went back: %d then %d", prev, c)
		}
		prev = c
	}
}

func TestStampsAreNeverTheSentinels(t *testing.T) {
	if Now() <= 1 || nowMonotonic() <= 1 || base < minBase {
		t.Fatalf("Now %d, monotonic %d, base %d: a stamp must be above the sentinels 0 and 1", Now(), nowMonotonic(), base)
	}
}

// cur is the one switch: the wall until the flip, the monotonic reading after.
func TestCurSelectsTheClock(t *testing.T) {
	r := Reading{Wall: 1_800_000_000_000_000_000, Mono: 7}
	if got := cur(r, false); got != r.Wall+skew {
		t.Errorf("cur(wall mode) = %d, want the wall reading", got)
	}
	if got := cur(r, true); got != base+7 {
		t.Errorf("cur(monotonic mode) = %d, want base + 7", got)
	}
	r2 := Reading{Wall: r.Wall + int64(75*time.Second), Mono: 7}
	if cur(r2, true) != cur(r, true) {
		t.Error("the monotonic mode followed a step of the wall")
	}
	if cur(r2, false) == cur(r, false) {
		t.Error("the wall mode ignored a step of the wall")
	}
}

// The seam steps the wall under a running monotonic clock. Before the flip Now
// follows the wall (the stepped tests are red); after it, it does not (green).
func TestSeamSteppedWall(t *testing.T) {
	wall, m := int64(1_800_000_000_000_000_000), int64(5_000_000_000)
	restore := SetSeam(func() Reading { return Reading{Wall: wall, Mono: m} })
	defer restore()
	a := Now()
	wall += int64(75 * time.Second)
	m += int64(10 * time.Millisecond)
	b := Now()
	if flipped {
		if b-a != int64(10*time.Millisecond) {
			t.Errorf("flipped: Now moved %v, want 10 ms (the wall's step must not count)", time.Duration(b-a))
		}
	} else if b-a != int64(75*time.Second) {
		t.Errorf("routing phase: Now moved %v, want the wall's 75 s", time.Duration(b-a))
	}
}

func TestSetSeamRestoresOnlyItsOwn(t *testing.T) {
	read := func(w int64) func() Reading { return func() Reading { return Reading{Wall: w, Mono: w} } }
	r1 := SetSeam(read(11))
	r2 := SetSeam(read(22))
	// A restore that is not the last one made does not remove the seam above it.
	r1()
	if got := cur(Reading{Wall: 22, Mono: 22}, flipped); Now() != got {
		t.Fatalf("Now = %d after r1(), want the second seam's %d", Now(), got)
	}
	r2() // puts back what was there when it was made: the first seam
	r1 = SetSeam(nil)
	_ = r1
	if Now() < minBase {
		t.Fatalf("Now = %d: the real clock is not back", Now())
	}
	// Nested, in order.
	a := SetSeam(read(33))
	b := SetSeam(read(44))
	b()
	if got := cur(Reading{Wall: 33, Mono: 33}, flipped); Now() != got {
		t.Fatalf("Now = %d after b(), want the outer seam's %d", Now(), got)
	}
	a()
	if Now() < minBase {
		t.Fatalf("Now = %d: the real clock is not back", Now())
	}
}

func TestClampBase(t *testing.T) {
	for _, tc := range []struct{ in, want int64 }{
		{0, minBase},
		{30_000_000_000, minBase}, // a host that booted at 1970 + 30 s
		{minBase - 1, minBase},
		{1_800_000_000_000_000_000, 1_800_000_000_000_000_000},
		{maxBase + 1, maxBase},
		{math.MaxInt64, maxBase},
		{-5, minBase},
	} {
		if got := clampBase(tc.in); got != tc.want {
			t.Errorf("clampBase(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if y := time.Unix(0, minBase).Year(); y < 2000 || y > 2010 {
		t.Errorf("minBase is the year %d, want about 2006", y)
	}
	if y := time.Unix(0, maxBase).Year(); y < 2100 {
		t.Errorf("maxBase is the year %d, want about 2116", y)
	}
}

// FromWall keeps the time left, subtracts the REAL wall (not the seam's), and
// never returns a sentinel for an input that is not one.
func TestFromWallKeepsTheTimeLeft(t *testing.T) {
	for _, d := range []time.Duration{10 * time.Second, -10 * time.Second, 0} {
		w := time.Now().Add(d).UnixNano()
		got := FromWall(w) - Now()
		if abs(got-int64(d)) > tol {
			t.Errorf("FromWall(now%+v) - Now() = %v", d, time.Duration(got))
		}
	}
}

func TestFromWallPassesTheSentinels(t *testing.T) {
	for _, w := range []int64{0, 1, -1, math.MinInt64} {
		if got := FromWall(w); got != w {
			t.Errorf("FromWall(%d) = %d: the sentinels and non-positive inputs pass through", w, got)
		}
	}
}

// A clock far below the deadline's wall (a host that started at 1970, a seam
// that steps back) must not make a past deadline read as 0 or 1.
func TestFromWallOfAPastDeadlineIsNotASentinel(t *testing.T) {
	restore := SetSeam(func() Reading { return Reading{Wall: 100, Mono: 100} })
	defer restore()
	for _, w := range []int64{2, 3, 1000, time.Now().Add(-time.Hour).UnixNano()} {
		if got := FromWall(w); got < 2 {
			t.Errorf("FromWall(%d) = %d: an input above 1 must not come back as a sentinel", w, got)
		}
	}
}

func TestFromWallSaturates(t *testing.T) {
	// The clock is ahead of the real wall (a seam that stepped forward, or the
	// canary's skew), so that clock + (w - wall) overflows for a far deadline.
	restore := SetSeam(func() Reading {
		return Reading{Wall: time.Now().UnixNano() + int64(time.Hour), Mono: int64(time.Since(start))}
	})
	defer restore()
	for _, w := range []int64{math.MaxInt64, math.MaxInt64 - 1, 1 << 62, 1<<63 - 1<<40} {
		if got := FromWall(w); got < Now() {
			t.Errorf("FromWall(%d) = %d wrapped below Now", w, got)
		}
	}
}

// The seam steps the clock; the caller's reading of the wall is the real one,
// as in production. A deadline the caller computed from the real wall is the
// idle time left after the clock's NEW reading, not a seam-step too early or
// late: the stock websocket middleware is testable under a stepped seam.
func TestFromWallSubtractsTheRealWall(t *testing.T) {
	step := int64(75 * time.Second)
	restore := SetSeam(func() Reading {
		return Reading{Wall: time.Now().UnixNano() + step, Mono: int64(time.Since(start))}
	})
	defer restore()
	idle := int64(30 * time.Second)
	got := FromWall(time.Now().UnixNano()+idle) - Now()
	if abs(got-idle) > tol {
		t.Fatalf("FromWall under a stepped seam leaves %v, want the idle time %v", time.Duration(got), time.Duration(idle))
	}
}

func TestTimeCarriesTheTrueWallAndAMonotonicReading(t *testing.T) {
	age := 5 * time.Second
	got := Time(Now() - int64(age))
	if d := time.Since(got) - age; d < -time.Second || d > time.Second {
		t.Fatalf("time.Since(Time(Now()-5s)) = %v", time.Since(got))
	}
	if d := abs(time.Now().Add(-age).UnixNano() - got.UnixNano()); d > tol {
		t.Fatalf("Time's wall instant is %v off", time.Duration(d))
	}
	if !strings.Contains(got.String(), "m=") {
		t.Fatalf("Time(...) = %v carries no monotonic reading", got)
	}
	// Two stamps' difference is the clock's difference (each call reads the
	// clock once, microseconds apart).
	a := Now()
	if d := Time(a+int64(time.Second)).Sub(Time(a)) - time.Second; d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("Time does not preserve differences: off by %v", d)
	}
}

// A stepped seam moves Now and the age of a stamp with it, but the wall time of
// the stamp Time returns is the real one (the logger prints it).
func TestTimeUnderASteppedSeamIsStillTheRealWall(t *testing.T) {
	step := int64(75 * time.Second)
	restore := SetSeam(func() Reading {
		return Reading{Wall: time.Now().UnixNano() + step, Mono: int64(time.Since(start))}
	})
	defer restore()
	stamp := Now() // taken on the stepped clock
	time.Sleep(20 * time.Millisecond)
	got := Time(stamp)
	if flipped {
		if d := abs(time.Now().Add(-20*time.Millisecond).UnixNano() - got.UnixNano()); d > tol {
			t.Fatalf("flipped: the stamp's wall instant is %v off", time.Duration(d))
		}
	}
	// Until the flip the clock IS the stepped wall, so the age is measured on it.
	if age := time.Since(got); age < 15*time.Millisecond || age > time.Second {
		t.Fatalf("age of the stamp = %v, want about 20 ms", age)
	}
}

func TestFromTimeReusesTheCallersReading(t *testing.T) {
	tm := time.Now()
	if d := FromTime(tm) - Now(); abs(d) > tol {
		t.Fatalf("FromTime(now) - Now() = %v", time.Duration(d))
	}
	old := time.Now().Add(-time.Hour)
	if d := Now() - FromTime(old); abs(d-int64(time.Hour)) > tol {
		t.Fatalf("an hour-old instant is %v old on the clock", time.Duration(d))
	}
	// The monotonic form: no clock read, t's own reading against start.
	if d := fromTimeMonotonic(tm) - nowMonotonic(); abs(d) > tol {
		t.Fatalf("the monotonic FromTime is %v from nowMonotonic", time.Duration(d))
	}
}

// FromTime agrees with Now under a seam that steps: the result is Now minus the
// real age of t.
func TestFromTimeAgreesWithNowUnderASeam(t *testing.T) {
	restore := SetSeam(func() Reading {
		return Reading{Wall: time.Now().UnixNano() + int64(75*time.Second), Mono: int64(time.Since(start))}
	})
	defer restore()
	tm := time.Now()
	if d := FromTime(tm) - Now(); abs(d) > tol {
		t.Fatalf("under a stepped seam FromTime(now) - Now() = %v", time.Duration(d))
	}
	old := time.Now().Add(-time.Hour)
	if d := Now() - FromTime(old); abs(d-int64(time.Hour)) > tol {
		t.Fatalf("under a stepped seam an hour-old instant is %v old on the clock", time.Duration(d))
	}
}

func TestZeroAllocations(t *testing.T) {
	tm := time.Now()
	for name, f := range map[string]func(){
		"Now":      func() { _ = Now() },
		"FromWall": func() { _ = FromWall(tm.UnixNano() + 5) },
		"Time":     func() { _ = Time(Now()) },
		"FromTime": func() { _ = FromTime(tm) },
	} {
		if n := testing.AllocsPerRun(100, f); n != 0 {
			t.Errorf("%s allocates %v", name, n)
		}
	}
}

func TestUntil(t *testing.T) {
	for _, d := range []time.Duration{10 * time.Second, -10 * time.Second, 0} {
		if got := Until(Now() + int64(d)); got < d-time.Second || got > d+time.Second {
			t.Errorf("Until(Now()%+v) = %v", d, got)
		}
	}
	// Under a stepped seam the answer follows the clock, not the real wall.
	restore := SetSeam(func() Reading {
		return Reading{Wall: time.Now().UnixNano() + int64(75*time.Second), Mono: int64(time.Since(start))}
	})
	defer restore()
	if got := Until(Now() + int64(5*time.Second)); got < 4*time.Second || got > 6*time.Second {
		t.Errorf("under a stepped seam Until(Now()+5s) = %v", got)
	}
}

// The stepped clocks tests of other packages use: the wall reading steps, the
// monotonic one runs.
func TestSteppedClocks(t *testing.T) {
	var st Stepper
	restore := SetStepper(&st)
	a := Now()
	st.Add(75 * time.Second)
	b := Now()
	restore()
	if flipped {
		if d := time.Duration(b - a); d < 0 || d > time.Second {
			t.Fatalf("flipped: a step of the wall moved Now by %v", d)
		}
	} else if d := time.Duration(b - a); d < 74*time.Second || d > 76*time.Second {
		t.Fatalf("routing phase: Now moved %v, want the wall's 75 s", d)
	}
	if st.Offset() != 75*time.Second {
		t.Fatalf("offset = %v", st.Offset())
	}
	restore = SetStair(time.Minute, 20*time.Millisecond)
	defer restore()
	t0 := Now()
	time.Sleep(110 * time.Millisecond)
	d := time.Duration(Now() - t0)
	if flipped {
		if d > time.Second {
			t.Fatalf("flipped: the stair moved Now by %v", d)
		}
	} else if d < 3*time.Minute {
		t.Fatalf("routing phase: the stair moved Now by only %v in 110 ms (every 20 ms by a minute)", d)
	}
}
