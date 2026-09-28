//go:build linux

package iouring

// The connState field the celeris#750 tests read.

// heldSends750 reports how many of cs's send completions are held for its
// dispatch goroutine's hand-back. Worker thread (the test's).
func heldSends750(cs *connState) int { return len(cs.heldSends) }
