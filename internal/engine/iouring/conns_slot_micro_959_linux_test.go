//go:build linux

package iouring

import "testing"

// BenchmarkConnSlotWrite959 isolates what celeris#959 adds to the accept and
// close paths: the slot store goes from bare to one uncontended connsMu
// lock/unlock around it. Both arms run the same slice stores (one install and
// one clear per op, as a connection's life does).
func BenchmarkConnSlotWrite959(b *testing.B) {
	cs := &connState{}
	w := &Worker{conns: make([]*connState, 1024)}
	b.Run("bare", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			w.conns[i&1023] = cs
			w.conns[i&1023] = nil
		}
	})
	b.Run("locked", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			w.setConnSlot(i&1023, cs)
			w.clearConnSlot(i & 1023)
		}
	})
}
