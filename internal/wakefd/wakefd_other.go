//go:build !unix

// Package wakefd owns an event loop's wakeup eventfd. On a platform without
// unix descriptors it holds no eventfd at all: see [WakeFD].
package wakefd

// WakeFD on a platform without unix descriptors (windows). The engines that
// own a wakeup eventfd (epoll, io_uring) are Linux-only, so no loop on this
// platform ever holds one; the type exists so that internal/conn, which hands
// a *WakeFD to its H2 state, compiles everywhere the root package does.
//
// It is the permanently disabled handle that wakefd.go documents for a nil
// *WakeFD, made concrete: Signal and Close are no-ops, FD reports -1 and Set
// reports false, so the caller keeps (and closes) whatever it passed.
type WakeFD struct{}

// New returns a disabled handle. fd is ignored: this platform has no eventfd
// to own, and the caller keeps fd.
func New(_ int) *WakeFD { return &WakeFD{} }

// Signal does nothing: there is no loop to wake.
func (w *WakeFD) Signal() {}

// Set reports false: a handle on this platform never takes ownership of a
// descriptor, so the caller still owns fd and must close it.
func (w *WakeFD) Set(_ int) bool { return false }

// FD returns -1: there is no descriptor.
func (w *WakeFD) FD() int { return -1 }

// Close does nothing.
func (w *WakeFD) Close() {}
