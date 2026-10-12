package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// dialConnNetConn is dialConn for a loop that adopts a net.Conn instead of a
// file descriptor (eventloop.NetConnRegistrar): the transport of platforms
// without unix descriptors (windows), where the portable
// goroutine-per-connection loop reads the conn and calls onRecv. It is
// compiled everywhere so that its tests run on Linux CI as well; only
// conn_other.go selects it as dialConn.
//
// The conn has no descriptor: c.fd holds the identifier the loop assigned,
// c.fdFile stays nil, and the loop closes the net.Conn on UnregisterConn
// (Close) and right after onClose. The pgConn literal below mirrors the one
// in dialConn (conn_unix.go).
//
//nolint:unparam // closeLoop is always nil today but retained for pool reuse, as in dialConn
func dialConnNetConn(ctx context.Context, prov engine.EventLoopProvider, closeLoop func(), dsn DSN, workerHint int) (*pgConn, error) {
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

	nw := prov.NumWorkers()
	if nw <= 0 {
		_ = tcp.Close()
		return nil, errors.New("celeris-postgres: event loop has 0 workers")
	}
	// The unix dial spreads conns over the workers by descriptor; a conn with
	// no descriptor has nothing to spread by, so an unusable hint (the
	// database/sql connector always passes -1) lands on worker 0. The
	// portable worker runs one goroutine per conn, so the shared map lock is
	// the only cost.
	if workerHint < 0 || workerHint >= nw {
		workerHint = 0
	}
	loop := prov.WorkerLoop(workerHint)
	reg, ok := loop.(eventloop.NetConnRegistrar)
	if !ok {
		_ = tcp.Close()
		return nil, fmt.Errorf("celeris-postgres: event loop worker %T cannot adopt a net.Conn", loop)
	}

	c := &pgConn{
		loop:      loop,
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
		pending:      make([]*pgRequest, 0, 8),
		createdAt:    time.Now(),
		serverParams: map[string]string{},
	}
	c.onRecvFn = c.onRecv
	c.onCloseFn = c.onClose
	c.lastUsedAt.Store(time.Now().UnixNano())

	id, err := reg.RegisterNetConn(tcp, c.onRecvFn, c.onCloseFn)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	// The loop may already have fired onClose, which reads c.fd under fdMu
	// (shutdownFD): publish the identifier under it. onRecv reads c.fd too
	// (writeRawFromRecv, under recvWriteMu, not fdMu); that read needs no lock
	// here only because the server sends nothing before the StartupMessage,
	// which doStartup writes after this publish, so no onRecv can run before
	// it (an onRecv that did would be a data race on c.fd).
	c.fdMu.Lock()
	c.fd = id
	c.fdMu.Unlock()

	if err := c.doStartup(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
