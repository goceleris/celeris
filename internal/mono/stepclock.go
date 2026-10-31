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
