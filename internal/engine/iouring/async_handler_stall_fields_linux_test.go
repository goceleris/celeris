//go:build linux

package iouring

// The connState fields the celeris#704 tests set or read.

// setParked704 marks cs's dispatch goroutine parked (waiting for input, so
// not holding detachMu across a handler) or not. Caller holds asyncInMu.
func setParked704(cs *connState, parked bool) { cs.asyncParked = parked }

// relinkOwed704 reports whether cs's dispatch goroutine owes the worker a
// hand-back of a conn the dirty pass gave up.
func relinkOwed704(cs *connState) bool {
	cs.asyncInMu.Lock()
	defer cs.asyncInMu.Unlock()
	return cs.relinkOwed
}
