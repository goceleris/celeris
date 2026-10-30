package drainclock

import (
	"testing"
	"time"
)

func TestNowIsMonotonicAndNeverZero(t *testing.T) {
	a := Now()
	time.Sleep(2 * time.Millisecond)
	b := Now()
	if a < 1 || b <= a {
		t.Fatalf("Now = %d then %d: want positive and increasing", a, b)
	}
	if b-a > int64(time.Second) {
		t.Fatalf("Now moved %v in 2 ms", time.Duration(b-a))
	}
}

// A step of the wall clock does not move Now: the seam's wall reading steps by
// a minute and a quarter and Now follows the monotonic one.
func TestNowIgnoresTheWallClock(t *testing.T) {
	var wall int64 = 1_700_000_000_000_000_000
	var mono int64 = 5_000_000_000
	restore := SetSeam(func() Reading { return Reading{Wall: wall, Mono: mono} })
	defer restore()
	if got := Now(); got != mono {
		t.Fatalf("Now = %d, want the monotonic reading %d", got, mono)
	}
	wall += int64(75 * time.Second)
	if got := Now(); got != mono {
		t.Fatalf("Now = %d after a forward step of the wall clock, want %d", got, mono)
	}
	wall -= int64(150 * time.Second)
	if got := Now(); got != mono {
		t.Fatalf("Now = %d after a backward step of the wall clock, want %d", got, mono)
	}
}

// At puts a deadline on the Now scale: a deadline with a monotonic reading by
// the monotonic time left to it, one without (an instant of the wall clock) by
// the wall time left to it.
func TestAtPutsADeadlineOnTheNowScale(t *testing.T) {
	const tol = int64(50 * time.Millisecond)
	for _, tc := range []struct {
		name string
		d    time.Time
	}{
		{"with-monotonic-reading", time.Now().Add(30 * time.Second)},
		{"wall-clock-instant", time.Now().Round(0).Add(30 * time.Second)},
	} {
		got := At(tc.d) - Now()
		if diff := got - int64(30*time.Second); diff > tol || diff < -tol {
			t.Errorf("%s: At(d) - Now() = %v, want about 30s", tc.name, time.Duration(got))
		}
	}
}

func TestSetSeamRestoresOnlyItsOwn(t *testing.T) {
	r1 := SetSeam(func() Reading { return Reading{Mono: 1} })
	r2 := SetSeam(func() Reading { return Reading{Mono: 2} })
	r1() // an older caller's restore does not undo the newer one
	if got := Now(); got != 2 {
		t.Fatalf("Now = %d after the older restore, want the newer seam's 2", got)
	}
	r2() // puts back what it replaced, the older seam
	if got := Now(); got != 1 {
		t.Fatalf("Now = %d after the newer restore, want the seam it replaced (1)", got)
	}
	r1()
	if got := Now(); got < 1_000 {
		t.Fatalf("Now = %d with every seam restored, want the real clock", got)
	}
}

// A deadline further away than time.Duration can count does not wrap.
func TestAtSaturatesForAFarDeadline(t *testing.T) {
	for _, d := range []time.Time{time.Date(2600, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if got := At(d); got < Now()+int64(100*365*24*time.Hour) {
			t.Errorf("At(%v) = %d: wrapped or ended at once", d, got)
		}
	}
	if got := At(time.Time{}); got >= Now() {
		t.Errorf("At(zero) = %d, want a point in the past", got)
	}
}
