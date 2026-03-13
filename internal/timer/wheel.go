// Package timer implements a hierarchical timer wheel for efficient
// timeout management in event-driven I/O engines.
package timer

import (
	"sync"
	"time"
)

const (
	// WheelSize is the number of slots in the timer wheel (power of 2 for fast modulo).
	WheelSize = 8192
	// TickInterval is the granularity of the timer wheel.
	TickInterval = 100 * time.Millisecond

	wheelMask = WheelSize - 1
)

// TimeoutKind identifies the type of timeout.
type TimeoutKind uint8

// Timeout kinds for the timer wheel.
const (
	ReadTimeout TimeoutKind = iota
	WriteTimeout
	IdleTimeout
)

// Entry represents a scheduled timeout.
type Entry struct {
	FD       int
	Deadline int64 // unix nano
	Kind     TimeoutKind
	next     *Entry
}

var entryPool = sync.Pool{New: func() any { return &Entry{} }}

func getEntry() *Entry {
	return entryPool.Get().(*Entry)
}

func putEntry(e *Entry) {
	e.FD = 0
	e.Deadline = 0
	e.Kind = 0
	e.next = nil
	entryPool.Put(e)
}

// Wheel is a hashed timer wheel with O(1) schedule and cancel.
type Wheel struct {
	slots    [WheelSize]*Entry
	current  uint64
	startNs  int64
	onExpire func(fd int, kind TimeoutKind)
	// fdSlots tracks the slot index for each FD to enable O(1) cancel.
	fdSlots map[int]int64 // fd → deadline
}

// New creates a timer wheel that calls onExpire for each expired entry.
func New(onExpire func(fd int, kind TimeoutKind)) *Wheel {
	return &Wheel{
		startNs:  time.Now().UnixNano(),
		onExpire: onExpire,
		fdSlots:  make(map[int]int64),
	}
}

func (w *Wheel) slotIndex(deadline int64) uint64 {
	tick := uint64(deadline-w.startNs) / uint64(TickInterval)
	return tick & wheelMask
}

// Schedule registers a timeout for the given FD. Any previous timeout for
// the same FD is logically cancelled (the old entry is ignored on expiry).
func (w *Wheel) Schedule(fd int, timeout time.Duration, kind TimeoutKind) {
	w.ScheduleAt(fd, timeout, kind, time.Now().UnixNano())
}

// ScheduleAt is like Schedule but accepts a pre-computed now timestamp (UnixNano)
// to avoid redundant time.Now() calls when scheduling multiple timeouts in the
// same request processing cycle.
func (w *Wheel) ScheduleAt(fd int, timeout time.Duration, kind TimeoutKind, nowNs int64) {
	deadline := nowNs + int64(timeout)
	slot := w.slotIndex(deadline)

	e := getEntry()
	e.FD = fd
	e.Deadline = deadline
	e.Kind = kind
	e.next = w.slots[slot]
	w.slots[slot] = e

	w.fdSlots[fd] = deadline
}

// Cancel removes any pending timeout for the given FD.
func (w *Wheel) Cancel(fd int) {
	delete(w.fdSlots, fd)
}

// Tick advances the wheel and fires expired entries. Returns the number
// of entries that expired. Call this from the event loop after processing
// I/O events.
func (w *Wheel) Tick() int {
	now := time.Now().UnixNano()
	currentTick := uint64(now-w.startNs) / uint64(TickInterval)
	count := 0

	for w.current <= currentTick {
		slot := w.current & wheelMask
		var prev *Entry
		e := w.slots[slot]
		for e != nil {
			next := e.next
			if e.Deadline <= now {
				// Check if this entry is still the active one for this FD.
				if active, ok := w.fdSlots[e.FD]; ok && active == e.Deadline {
					delete(w.fdSlots, e.FD)
					w.onExpire(e.FD, e.Kind)
					count++
				}
				// Remove from list.
				if prev == nil {
					w.slots[slot] = next
				} else {
					prev.next = next
				}
				putEntry(e)
			} else {
				prev = e
			}
			e = next
		}
		w.current++
	}

	return count
}

// Len returns the number of tracked FDs (approximate).
func (w *Wheel) Len() int {
	return len(w.fdSlots)
}
