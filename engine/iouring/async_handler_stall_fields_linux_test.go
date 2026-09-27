//go:build linux

package iouring

// The connState fields the celeris#704 tests set or read. main has neither, so
// on main these are stubs: no goroutine is ever marked parked (the tests' only
// use of it is to mark one parked for the bounded-holder control, which main
// waits on either way), and no hand-back is ever owed. The fix replaces this
// file with the real accessors.

func setParked704(*connState, bool) {}

func relinkOwed704(*connState) bool { return false }
