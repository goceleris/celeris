//go:build linux

package iouring

// The conn table, w.conns, is the worker goroutine's: it reads and writes it
// with no lock, and everything the worker does with a slot (the dispatch of a
// CQE, the timeout sweep, the close paths) stays that way. One reader is not
// the worker: RegisterConn, on a driver's goroutine, asks whether a descriptor
// number is an HTTP connection's, and answered from the slot (celeris#959; the
// epoll engine's twin was celeris#775). So every write of a slot goes through
// setConnSlot or clearConnSlot, which take connsMu around the store alone, and
// RegisterConn reads through connSlotBusy under the same lock; a test pins
// that no other code writes a slot (TestConnSlotWritesGoThroughTheHelpers959).
// len(w.conns) is fixed when the worker is built and needs no lock.
//
// connsMu is a leaf. It is held for one store or one load: never across a
// callback (OnConnect, OnDisconnect), a syscall, or another lock, and nothing
// is acquired while it is held, so it can sit under any lock the worker or a
// driver goroutine holds (RegisterConn takes it under driverMu) without a
// cycle. It is not driverMu, which the epoll engine's fix uses, because the
// io_uring worker would then wait behind every driver Write and every CQE
// lookup of a driver conn on its accept and close paths.
//
// Order at a close: the slot is cleared before the descriptor is closed (as
// finishClose has always done), so a driver that is handed the freed number
// reads an empty slot and is not refused for a connection that is gone.

// setConnSlot installs cs as the HTTP connection on descriptor number fd.
// Worker goroutine only.
func (w *Worker) setConnSlot(fd int, cs *connState) {
	w.connsMu.Lock()
	w.conns[fd] = cs
	w.connsMu.Unlock()
}

// clearConnSlot empties fd's slot. Worker goroutine only.
func (w *Worker) clearConnSlot(fd int) {
	w.connsMu.Lock()
	w.conns[fd] = nil
	w.connsMu.Unlock()
}

// connSlotBusy reports whether fd is an HTTP connection's number on this
// worker: the one read of the table off the worker goroutine.
func (w *Worker) connSlotBusy(fd int) bool {
	if fd < 0 || fd >= len(w.conns) {
		return false
	}
	w.connsMu.Lock()
	busy := w.conns[fd] != nil
	w.connsMu.Unlock()
	return busy
}
