//go:build !linux && unix

package eventloop

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// worker is the goroutine-per-connection fallback used on non-Linux unix
// hosts (darwin) for development and testing. Everything it does with a
// connection is in portableWorker; RegisterConn, which turns a descriptor
// into a net.Conn, is what this file adds.
type worker struct {
	portableWorker
}

var _ NetConnRegistrar = (*worker)(nil)

func newLoop(workers int) (*Loop, error) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Loop{cancel: cancel, workers: make([]loopWorker, workers)}
	for i := 0; i < workers; i++ {
		l.workers[i] = &worker{portableWorker: newPortableWorker(ctx, i)}
	}
	return l, nil
}

// RegisterConn wraps fd in a net.Conn via [net.FileConn] and spawns a reader
// goroutine that invokes onRecv until the conn closes or onClose fires.
//
// Note: net.FileConn dup(2)s the descriptor, so the Loop holds an independent
// reference. The caller still owns the original fd and must close it after
// UnregisterConn.
func (w *worker) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	_, err := w.register(fd, false, func() (net.Conn, error) {
		// Dup the fd so the Loop owns an independent file descriptor. Wrapping
		// the caller's fd directly in os.NewFile creates a second *os.File for
		// the same kernel fd — both carry finalizers, and the first GC run
		// races with the caller's own close path, producing sporadic "bad file
		// descriptor" errors on writes.
		dupFd, err := unix.Dup(fd)
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(dupFd), "celeris-driver-fd")
		if f == nil {
			_ = syscall.Close(dupFd)
			return nil, errors.New("celeris/eventloop: invalid fd")
		}
		c, err := net.FileConn(f)
		// net.FileConn dups f internally; close f now to release the extra
		// descriptor regardless of success or failure.
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return c, nil
	}, onRecv, onClose)
	return err
}
