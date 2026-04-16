//go:build !linux

package eventloop

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// worker is a goroutine-per-connection fallback used on non-Linux hosts
// (darwin, windows) for development and testing.
type worker struct {
	id     int
	mu     sync.RWMutex
	conns  map[int]*driverConn
	closed atomic.Bool
	ctx    context.Context
	wg     sync.WaitGroup
}

type driverConn struct {
	fd      int
	conn    net.Conn
	onRecv  func([]byte)
	onClose func(error)

	writeMu sync.Mutex
	mu      sync.Mutex
	closing bool
	closed  bool
	done    chan struct{}
}

func newLoop(workers int) (*Loop, error) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Loop{cancel: cancel, workers: make([]loopWorker, workers)}
	for i := 0; i < workers; i++ {
		l.workers[i] = &worker{
			id:    i,
			conns: make(map[int]*driverConn),
			ctx:   ctx,
		}
	}
	return l, nil
}

func (w *worker) shutdown() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	w.mu.Lock()
	conns := make([]*driverConn, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
	}
	w.conns = map[int]*driverConn{}
	w.mu.Unlock()
	for _, c := range conns {
		c.shutdownConn(ErrLoopClosed)
	}
	w.wg.Wait()
	return nil
}

// CPUID reports the CPU the worker is pinned to. The fallback does not pin.
func (w *worker) CPUID() int { return -1 }

// RegisterConn wraps fd in a net.Conn via [net.FileConn] and spawns a reader
// goroutine that invokes onRecv until the conn closes or onClose fires.
//
// Note: net.FileConn dup(2)s the descriptor, so the Loop holds an independent
// reference. The caller still owns the original fd and must close it after
// UnregisterConn.
func (w *worker) RegisterConn(fd int, onRecv func([]byte), onClose func(error)) error {
	if w.closed.Load() {
		return ErrLoopClosed
	}
	if fd < 0 {
		return errors.New("celeris/eventloop: negative fd")
	}
	w.mu.Lock()
	if _, ok := w.conns[fd]; ok {
		w.mu.Unlock()
		return ErrAlreadyRegistered
	}
	// Placeholder to reject concurrent registration of the same fd. The
	// real struct replaces it below once the conn is constructed.
	w.conns[fd] = &driverConn{fd: fd}
	w.mu.Unlock()

	// Dup the fd so the Loop owns an independent file descriptor. Wrapping
	// the caller's fd directly in os.NewFile creates a second *os.File for
	// the same kernel fd — both carry finalizers, and the first GC run
	// races with the caller's own close path, producing sporadic "bad file
	// descriptor" errors on writes.
	dupFd, err := unix.Dup(fd)
	if err != nil {
		w.mu.Lock()
		delete(w.conns, fd)
		w.mu.Unlock()
		return err
	}
	f := os.NewFile(uintptr(dupFd), "celeris-driver-fd")
	if f == nil {
		_ = syscall.Close(dupFd)
		w.mu.Lock()
		delete(w.conns, fd)
		w.mu.Unlock()
		return errors.New("celeris/eventloop: invalid fd")
	}
	c, err := net.FileConn(f)
	// net.FileConn dups f internally; close f now to release the extra
	// descriptor regardless of success or failure.
	_ = f.Close()
	if err != nil {
		w.mu.Lock()
		delete(w.conns, fd)
		w.mu.Unlock()
		return err
	}
	dc := &driverConn{
		fd:      fd,
		conn:    c,
		onRecv:  onRecv,
		onClose: onClose,
		done:    make(chan struct{}),
	}
	w.mu.Lock()
	w.conns[fd] = dc
	w.mu.Unlock()

	w.wg.Add(1)
	go w.readerLoop(dc)
	return nil
}

func (w *worker) readerLoop(c *driverConn) {
	defer w.wg.Done()
	buf := make([]byte, 16<<10)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 && c.onRecv != nil {
			c.onRecv(buf[:n])
		}
		if err != nil {
			// Normal teardown: UnregisterConn closed the conn.
			c.mu.Lock()
			closing := c.closing
			c.mu.Unlock()
			if closing {
				c.fire(nil)
				return
			}
			c.fire(err)
			w.mu.Lock()
			delete(w.conns, c.fd)
			w.mu.Unlock()
			return
		}
	}
}

// UnregisterConn closes the wrapped net.Conn, which unblocks the reader
// goroutine; onClose(nil) fires once the reader observes the close.
func (w *worker) UnregisterConn(fd int) error {
	w.mu.Lock()
	c, ok := w.conns[fd]
	if ok {
		delete(w.conns, fd)
	}
	w.mu.Unlock()
	if !ok {
		return engine.ErrUnknownFD
	}
	c.shutdownConn(nil)
	return nil
}

// Write serializes per-FD via writeMu; the standalone fallback has no
// cross-conn queue so ErrQueueFull is never returned here.
func (w *worker) Write(fd int, data []byte) error {
	if w.closed.Load() {
		return ErrLoopClosed
	}
	w.mu.RLock()
	c, ok := w.conns[fd]
	w.mu.RUnlock()
	if !ok || c.conn == nil {
		return engine.ErrUnknownFD
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.closing || c.closed {
		c.mu.Unlock()
		return engine.ErrUnknownFD
	}
	c.mu.Unlock()
	_, err := c.conn.Write(data)
	return err
}

// shutdownConn marks the conn closing and closes the underlying net.Conn. The
// reader goroutine observes the close and fires onClose.
func (c *driverConn) shutdownConn(err error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		c.fire(err)
	}
}

// fire invokes onClose exactly once.
func (c *driverConn) fire(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	cb := c.onClose
	c.mu.Unlock()
	if cb != nil {
		cb(err)
	}
}
