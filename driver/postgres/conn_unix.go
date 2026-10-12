//go:build unix

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// dialConn dials a TCP connection to the DSN's host:port, sets it non-blocking,
// and registers it with the given event loop. The returned pgConn is fully
// initialized (including startup) and ready for queries.
// dialConn accepts closeLoop for symmetry with Pool.dialConn; callers that
// own the loop themselves (e.g. [Connector.Connect]) pass nil.
func dialConn(ctx context.Context, prov engine.EventLoopProvider, closeLoop func(), dsn DSN, workerHint int) (*pgConn, error) {
	if err := dsn.CheckSSL(); err != nil {
		return nil, err
	}
	addr, err := resolveAddr(dsn.Host, dsn.Port)
	if err != nil {
		return nil, err
	}

	dialTimeout := dsn.Options.ConnectTimeout
	dialer := net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		_ = raw.Close()
		return nil, fmt.Errorf("celeris-postgres: expected *net.TCPConn, got %T", raw)
	}
	_ = tcp.SetNoDelay(true)

	file, err := tcp.File()
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	// tcp.File dup'd the fd; close the net.TCPConn wrapper. We own the dup'd
	// fd from here on. CRITICAL: *os.File carries a runtime finalizer that
	// closes the fd on GC — if file becomes unreachable the finalizer shuts
	// the fd out from under the event loop (observed as sporadic EBADF /
	// hangs under GC pressure in high-rate benchmarks). Store file in the
	// pgConn so it stays reachable for the conn's lifetime; closeFDOnce()
	// closes via file.Close() so the finalizer is satisfied.
	fd := int(file.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = tcp.Close()
		_ = file.Close()
		return nil, err
	}
	tuneConnSocket(fd)
	_ = tcp.Close()

	nw := prov.NumWorkers()
	if nw <= 0 {
		// Use file.Close() (not syscall.Close(fd)) so the *os.File's
		// runtime finalizer is disarmed. Otherwise a later GC cycle would
		// call syscall.Close(fd) a second time, potentially on an
		// unrelated fd the kernel has since reassigned to another socket.
		_ = file.Close()
		return nil, errors.New("celeris-postgres: event loop has 0 workers")
	}
	if workerHint < 0 || workerHint >= nw {
		workerHint = fd % nw
		if workerHint < 0 {
			workerHint = -workerHint
		}
	}
	loop := prov.WorkerLoop(workerHint)

	// Cache the SyncRoundTripper capability for the fast path.
	var syncL *pgSyncLoop
	if srt, ok := loop.(syncRoundTripper); ok {
		syncL = &pgSyncLoop{sync: srt}
	}

	c := &pgConn{
		fd:        fd,
		fdFile:    file,
		loop:      loop,
		syncLoop:  syncL,
		workerID:  workerHint,
		closeLoop: closeLoop,
		addr:      addr,
		opts:      dsn.Options,
		dsn:       dsn,
		reader:    protocol.NewReader(),
		writer:    protocol.NewWriter(),
		bridge:    async.NewBridge(),
		stmtCache: newLRU(dsn.Options.StatementCacheSize),
		autoCache: dsn.Options.AutoCacheStatements && dsn.Options.StatementCacheSize > 0,
		// Pre-size pending queue for the common case (pipeline depth 1-2).
		// Backing cap is retained across enqueue/popHead cycles thanks to
		// the in-place shift in popHead, so bursty depths only allocate
		// on the first surge.
		pending:      make([]*pgRequest, 0, 8),
		createdAt:    time.Now(),
		serverParams: map[string]string{},
	}
	if syncL != nil {
		c.syncBuf = make([]byte, 16<<10) // 16 KiB read buffer for sync path
	}
	c.onRecvFn = c.onRecv
	c.onCloseFn = c.onClose
	c.lastUsedAt.Store(time.Now().UnixNano())

	if err := loop.RegisterConn(fd, c.onRecvFn, c.onCloseFn); err != nil {
		// Close via file.Close() to disarm the *os.File finalizer. A stray
		// syscall.Close(fd) here would leave the finalizer armed; when GC
		// later fires it would close the same fd a SECOND time, possibly
		// hitting an unrelated fd the kernel reassigned in the meantime.
		_ = file.Close()
		return nil, err
	}

	if err := c.doStartup(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
