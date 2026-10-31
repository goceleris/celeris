package mono

import (
	"sync/atomic"
	"time"
)

// A stepped clock for tests of other packages (the engines, conn, the root
// package). A test cannot step the machine's wall clock, so it steps the one the
// seam supplies: its monotonic reading runs as the real one does, its wall
// reading is the real one plus a step that the test (Stepper.Add) or a stair
// (SetStair) moves. Before the flip the clock IS the wall reading, so every
// deadline moves with a step; after it, none does. Tests only: the seam is
// process-wide.

// Stepper is a wall-clock step that tests add to.
type Stepper struct{ off atomic.Int64 }

// Add steps the wall reading of the clock by d (negative: backward).
func (s *Stepper) Add(d time.Duration) { s.off.Add(int64(d)) }

// Offset is the total step so far.
func (s *Stepper) Offset() time.Duration { return time.Duration(s.off.Load()) }

// SetStepper installs a seam whose wall reading is the real wall plus s's step
// and whose monotonic reading is the real one, until the returned function is
// called.
func SetStepper(s *Stepper) (restore func()) {
	return SetSeam(func() Reading {
		t := time.Now()
		return Reading{Wall: t.UnixNano() + s.off.Load(), Mono: int64(t.Sub(start))}
	})
}

// SetStair is SetStepper with a stair: from the first read of the clock, the
// wall reading steps by step every `every` of real time, cumulatively (a forward
// step of a minute and a quarter every 50 ms is more than any timeout a test
// waits for, so a deadline read from the wall is crossed within the first
// 50 ms; a backward one pushes it out of reach for as long).
func SetStair(step, every time.Duration) (restore func()) {
	var first atomic.Int64
	return SetSeam(func() Reading {
		t := time.Now()
		m := int64(t.Sub(start)) + 1
		first.CompareAndSwap(0, m)
		n := (m - first.Load()) / int64(every)
		return Reading{Wall: t.UnixNano() + n*int64(step), Mono: m - 1}
	})
}

// Manual is a clock that moves only when a test moves it, so that a test of a
// deadline needs no sleep and no margin for a slow runner: Advance moves both
// readings (time passes), Step moves the wall reading alone (the wall clock is
// stepped). Before the flip the clock is the wall reading, so a Step moves every
// deadline; after it, none. Tests only (SetManual).
type Manual struct {
	wall0 int64
	mono  atomic.Int64
	step  atomic.Int64
}

// NewManual returns a manual clock standing one hour past the real wall time on both
// readings. The hour is on purpose: a site that still reads the real clock
// (time.Now) instead of this package is then an hour behind every stamp of the clock,
// so a test of its deadline fails at once, where a clock that stood at the real time
// would agree with it to the millisecond and hide it.
func NewManual() *Manual {
	m := &Manual{wall0: time.Now().UnixNano()}
	m.mono.Store(int64(time.Hour))
	return m
}

// Advance lets d of time pass on both readings.
func (m *Manual) Advance(d time.Duration) { m.mono.Add(int64(d)) }

// Step steps the wall reading by d (negative: backward); the monotonic one stays.
func (m *Manual) Step(d time.Duration) { m.step.Add(int64(d)) }

// SetManual installs m as the clock until the returned function is called.
func SetManual(m *Manual) (restore func()) {
	return SetSeam(func() Reading {
		mo := m.mono.Load()
		return Reading{Wall: m.wall0 + mo + m.step.Load(), Mono: mo}
	})
}
