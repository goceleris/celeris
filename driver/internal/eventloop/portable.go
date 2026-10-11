package eventloop

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/engine"
)

// portableWorker is the goroutine-per-connection worker behind the non-Linux
// loop (darwin, windows): one reader goroutine per registered net.Conn. It
// holds nothing but net.Conn values, so it compiles on every platform and
// the platform files only decide how a connection reaches it: loop_other_unix.go
// turns a descriptor into a net.Conn (RegisterConn), and a caller on a
// platform without unix descriptors hands over the net.Conn itself
// ([NetConnRegistrar]). It is compiled on Linux as well, where no Loop uses
// it, so that its tests run on every CI platform.
//
// For development and testing only; the Linux epoll and io_uring workers are
// the production path.
type portableWorker struct {
	id     int
	mu     sync.RWMutex
	conns  map[int]*portableConn
	closed atomic.Bool
	ctx    context.Context
	wg     sync.WaitGroup

	// nextID is the last identifier handed to RegisterNetConn, guarded by
	// mu.
	nextID int
}

type portableConn struct {
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

// NetConnRegistrar is implemented by workers that adopt a net.Conn directly,
// which is how a caller on a platform without unix file descriptors (windows)
// hands its connection to the loop: there is no descriptor to register.
//
// RegisterNetConn spawns a reader goroutine that invokes onRecv until the
// conn closes or onClose fires, exactly as RegisterConn does for a
// descriptor. It returns the identifier to pass to Write and UnregisterConn
// (the engine.WorkerLoop methods take an int; here it names the connection
// and is never an operating-system descriptor). Identifiers are positive and
// are not reused while the connection is registered.
//
// The worker owns conn from a successful return on: UnregisterConn, a read
// error and Loop.Close all close it, and the caller must not. When
// RegisterNetConn returns an error the caller still owns conn.
type NetConnRegistrar interface {
	RegisterNetConn(conn net.Conn, onRecv func([]byte), onClose func(error)) (id int, err error)
}

// ErrNoFDRegistration is returned by RegisterConn on a worker that has no
// unix file descriptors to adopt (windows): a descriptor number cannot be
// turned into a net.Conn there, and the portable loop adopts the net.Conn
// itself through [NetConnRegistrar]. The drivers do that on such platforms
// and never call RegisterConn.
var ErrNoFDRegistration = errors.New("celeris/eventloop: registering a file descriptor is not supported on this platform; use RegisterNetConn")

// portableOnlyWorker is a portableWorker that completes engine.WorkerLoop
// with a RegisterConn that always fails: the worker of [NewPortable].
type portableOnlyWorker struct {
	portableWorker
}

var _ NetConnRegistrar = (*portableOnlyWorker)(nil)

// RegisterConn is not supported: see [ErrNoFDRegistration].
func (w *portableOnlyWorker) RegisterConn(int, func([]byte), func(error)) error {
	return ErrNoFDRegistration
}

// NewPortable creates a Loop whose workers are all portable
// goroutine-per-connection workers that adopt a net.Conn through
// [NetConnRegistrar], on every platform. New returns exactly this on a
// platform without unix file descriptors (windows); elsewhere it exists so
// that the drivers' net.Conn transport, the only one windows runs, can be
// tested on Linux and darwin too. If workers <= 0, runtime.NumCPU() is used.
// The caller must invoke [Loop.Close] when finished.
func NewPortable(workers int) (*Loop, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Loop{cancel: cancel, workers: make([]loopWorker, workers)}
	for i := range l.workers {
		l.workers[i] = &portableOnlyWorker{portableWorker: newPortableWorker(i, ctx)}
	}
	return l, nil
}

// netConnIDBase is the first identifier RegisterNetConn hands out. It sits
// above any descriptor number a process holds, so an identifier cannot be
// mistaken for a descriptor registered through RegisterConn on the same
// worker (darwin).
const netConnIDBase = 1 << 30

// IsNetConnID reports whether id is an identifier of a connection registered
// through [NetConnRegistrar] rather than a file descriptor number. A driver
// whose conn was adopted that way has no descriptor of its own to close or
// shut down: the worker closes the net.Conn.
func IsNetConnID(id int) bool { return id >= netConnIDBase }

// netConnIDMax bounds the identifiers so they fit an int on 32-bit targets.
// When nextID passes it, allocation starts again at netConnIDBase and skips
// identifiers still registered.
const netConnIDMax = 1<<31 - 1

func newPortableWorker(id int, ctx context.Context) portableWorker {
	return portableWorker{
		id:    id,
		conns: make(map[int]*portableConn),
		ctx:   ctx,
	}
}

func (w *portableWorker) shutdown() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	w.mu.Lock()
	conns := make([]*portableConn, 0, len(w.conns))
	for _, c := range w.conns {
		conns = append(conns, c)
	}
	w.conns = map[int]*portableConn{}
	w.mu.Unlock()
	for _, c := range conns {
		c.shutdownConn(ErrLoopClosed)
	}
	w.wg.Wait()
	return nil
}

// CPUID reports the CPU the worker is pinned to. The fallback does not pin.
func (w *portableWorker) CPUID() int { return -1 }

// RegisterNetConn satisfies [NetConnRegistrar].
func (w *portableWorker) RegisterNetConn(conn net.Conn, onRecv func([]byte), onClose func(error)) (int, error) {
	if conn == nil {
		return 0, errors.New("celeris/eventloop: nil conn")
	}
	return w.register(0, true, func() (net.Conn, error) { return conn, nil }, onRecv, onClose)
}

// register reserves a key in the worker's table, asks open for the net.Conn
// and starts its reader goroutine. With alloc set the key is a fresh
// identifier chosen under the lock; otherwise it is fd.
func (w *portableWorker) register(fd int, alloc bool, open func() (net.Conn, error), onRecv func([]byte), onClose func(error)) (int, error) {
	if w.closed.Load() {
		return 0, ErrLoopClosed
	}
	if !alloc && fd < 0 {
		return 0, errors.New("celeris/eventloop: negative fd")
	}
	w.mu.Lock()
	if alloc {
		fd = w.allocIDLocked()
		if fd == 0 {
			w.mu.Unlock()
			return 0, errors.New("celeris/eventloop: no free connection identifier")
		}
	} else if _, ok := w.conns[fd]; ok {
		w.mu.Unlock()
		return 0, ErrAlreadyRegistered
	}
	// Placeholder to reject concurrent registration of the same fd. The
	// real struct replaces it below once the conn is constructed.
	w.conns[fd] = &portableConn{fd: fd}
	w.mu.Unlock()

	c, err := open()
	if err != nil {
		w.mu.Lock()
		delete(w.conns, fd)
		w.mu.Unlock()
		return 0, err
	}
	dc := &portableConn{
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
	return fd, nil
}

// allocIDLocked returns an identifier no registered connection holds, or 0
// when the whole range is taken. Callers hold w.mu.
func (w *portableWorker) allocIDLocked() int {
	for tries := 0; tries < 1<<10; tries++ {
		w.nextID++
		if w.nextID < netConnIDBase || w.nextID > netConnIDMax {
			w.nextID = netConnIDBase
		}
		if _, used := w.conns[w.nextID]; !used {
			return w.nextID
		}
	}
	return 0
}

func (w *portableWorker) readerLoop(c *portableConn) {
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
			// Remove the entry only if it is still this conn's. onClose may
			// have led the owner to unregister and close the fd, and a new
			// conn may have registered the same number since (celeris#784).
			w.mu.Lock()
			if cur, ok := w.conns[c.fd]; ok && cur == c {
				delete(w.conns, c.fd)
			}
			w.mu.Unlock()
			// The conn failed on its own, so nothing will ever unregister
			// it: the entry is gone, and UnregisterConn for this number now
			// reports an unknown fd without closing anything. Close it here,
			// or a conn the worker owns outlives its reader (on windows, a
			// socket handle held until the garbage collector runs).
			c.shutdownConn(nil)
			return
		}
	}
}

// UnregisterConn closes the wrapped net.Conn, which unblocks the reader
// goroutine; onClose(nil) fires once the reader observes the close. The
// reader reads its own duplicate of fd, never fd's number, so the caller may
// close fd as soon as UnregisterConn returns (celeris#784). A read that
// completed before the close is still delivered: onRecv can run once more
// after UnregisterConn has returned.
func (w *portableWorker) UnregisterConn(fd int) error {
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
func (w *portableWorker) Write(fd int, data []byte) error {
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
func (c *portableConn) shutdownConn(err error) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	conn := c.conn
	c.mu.Unlock()
	// Report err before closing: the close makes the reader goroutine
	// observe a closing conn and fire onClose(nil), which would win the
	// once-only fire and turn Loop.Close's ErrLoopClosed into nil.
	if err != nil {
		c.fire(err)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// fire invokes onClose exactly once.
func (c *portableConn) fire(err error) {
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
