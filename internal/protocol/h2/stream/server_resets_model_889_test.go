package stream

import (
	"math/rand/v2"
	"testing"
)

// celeris#889, review round 1 of PR #1005: serverResets against a plain model.
// The example tests (TestServerResets889, TestServerResetsHoldAFullBurst889)
// pin the cases their author thought of; the off-by-ones of a sliding bitset
// are the ones nobody thought of (a slide that clears one position too few
// leaves a stale bit, so an identifier that was never reset reads as reset and
// its DATA is ignored). The model keeps every marked odd identifier in a map
// and the highest index marked. has(id) is true iff id was marked, is odd, and
// its index is inside the last serverResetWindow indexes; an identifier older
// than the window when it is marked is not recorded.

type resetModel889 struct {
	top    uint32 // one past the highest index marked; 0: none
	marked map[uint32]bool
}

func (m *resetModel889) inWindow(idx uint32) bool {
	return m.top != 0 && idx < m.top && m.top-1-idx < serverResetWindow
}

func (m *resetModel889) mark(id uint32) {
	if id&1 == 0 {
		return
	}
	idx := id >> 1
	if idx >= m.top {
		m.top = idx + 1
	} else if !m.inWindow(idx) {
		return
	}
	m.marked[id] = true
}

func (m *resetModel889) has(id uint32) bool {
	return id&1 == 1 && m.marked[id] && m.inWindow(id>>1)
}

func TestServerResetsMatchTheModel889(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		r := rand.New(rand.NewPCG(seed, seed*7))
		var s serverResets
		m := &resetModel889{marked: map[uint32]bool{}}
		base := uint32(1)
		for step := range 2000 {
			var id uint32
			switch r.IntN(6) {
			case 0: // a jump forward, sometimes past the window
				base += uint32(r.IntN(3 * serverResetWindow * 2))
				id = base
			case 1: // far behind
				if base > 4*serverResetWindow {
					id = base - uint32(r.IntN(4*serverResetWindow))
				} else {
					id = uint32(r.IntN(int(base) + 1))
				}
			default: // near the top
				d := int64(r.IntN(64)) - 32
				if int64(base)+d < 0 {
					id = 0
				} else {
					id = uint32(int64(base) + d)
				}
				if id > base {
					base = id
				}
			}
			s.mark(id)
			m.mark(id)
			// Probe a band around the top and some older ones.
			lo := int64(base) - 4*serverResetWindow - 8
			if lo < 0 {
				lo = 0
			}
			for q := lo; q <= int64(base)+8; q += int64(1 + r.IntN(3)) {
				if got, want := s.has(uint32(q)), m.has(uint32(q)); got != want {
					t.Fatalf("seed %d step %d (marked %d, top %d): has(%d) = %v, the model says %v", seed, step, id, base, q, got, want)
				}
			}
		}
	}
}
