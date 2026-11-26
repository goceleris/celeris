package postgres

import (
	"context"
	"database/sql/driver"
	"sync"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// Connector is a database/sql/driver.Connector that opens celeris-backed
// PostgreSQL connections. It implements driver.Connector and additionally
// offers a WithEngine method that rebinds the connector to a running
// celeris.Server's event loop.
type Connector struct {
	dsn DSN

	// engine and busyPoll decide the SO_BUSY_POLL budget of each connection:
	// busyPoll when at least a microsecond (WithSocketBusyPoll), else the server's
	// Config.SocketBusyPoll (WithEngine), else none (celeris#1228).
	engine   ServerProvider
	busyPoll time.Duration

	// provider is the event loop; resolved lazily on first Connect so that
	// sql.Open("celeris-postgres", dsn) does not spin up a standalone loop
	// until it's actually needed.
	provMu   sync.Mutex
	provider engine.EventLoopProvider
	ownsLoop bool
}

var _ driver.Connector = (*Connector)(nil)

// NewConnector parses dsn and returns a Connector. Callers typically reach
// this via sql.OpenDB(postgres.NewConnector(dsn)) or via sql.Open, which
// calls Driver.OpenConnector.
func NewConnector(dsn string) (*Connector, error) {
	return newConnector(dsn)
}

func newConnector(dsn string) (*Connector, error) {
	parsed, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if err := parsed.CheckSSL(); err != nil {
		return nil, err
	}
	// database/sql production entry point — default to auto-prepared+
	// cached statements unless the DSN explicitly opted out. Mirrors
	// pgx's QueryExecModeCacheStatement default. Lower-level dialConn
	// users (internal tests, raw DSN flows) keep the field unset.
	if !parsed.Options.autoCacheStatementsExplicit {
		parsed.Options.AutoCacheStatements = true
	}
	return &Connector{dsn: parsed}, nil
}

// WithEngine rebinds this Connector to sp's event loop. The returned
// Connector shares the DSN but replaces the provider; it does not own the
// loop (so Close does not tear it down).
//
// The connections it opens take the server's Config.SocketBusyPoll as their
// SO_BUSY_POLL budget (Linux; 0, off, by default), unless
// [Connector.WithSocketBusyPoll] set one of its own.
//
// The driver takes the server's event loop once, when it is opened: open it after
// srv.Addr() is non-nil to share the server's loops; opened earlier it uses the
// driver's own loop for its whole life.
//
// Which mode a connection uses is decided at each Connect:
//
//   - On the driver's own event loop: no engine, a server that has not started
//     when the Connector was bound (also once it has started), and the std
//     engine. Results stream and have no size cap.
//   - Direct (Go's netpoll, the mode of a [Pool] on a started server): the
//     Connector was bound to a started epoll, io_uring or adaptive server, whose
//     workers run an inline handler on the worker's own thread, so a handler
//     that opened a connection on the loop would wait for the startup reply on
//     the worker that has to read it (the server stopped answering before
//     v1.6.0). A handler on that server that is not inline (async) is direct
//     too. A connection dialled before the server started stays on the loop.
//
// A direct connection has the limits of a direct-mode Pool: a context that is
// cancelled with no deadline does not interrupt a statement that is running
// (a deadline does, and the error is the context's, context.DeadlineExceeded,
// not a *PGError with SQLSTATE 57014), a result is read whole and fails with
// [ErrResultTooBig] past 64 MiB (so there is no row streaming), LISTEN and
// UNLISTEN fail with [ErrDirectModeUnsupported], and
// [Connector.WithSocketBusyPoll] has no effect, as the connection is not on an
// event loop. The mode follows the server, not the caller: a background
// goroutine that uses the Connector gets the same connections. To keep
// streaming and the loop's behaviour, bind the Connector before the server
// starts, or use no engine.
func (c *Connector) WithEngine(sp ServerProvider) *Connector {
	clone := &Connector{dsn: c.dsn, engine: sp, busyPoll: c.busyPoll}
	if sp != nil {
		if p := sp.EventLoopProvider(); p != nil {
			clone.provider = p
			clone.ownsLoop = false
			return clone
		}
	}
	return clone
}

// WithSocketBusyPoll returns a Connector that is this one with d as the
// SO_BUSY_POLL budget (Linux) of the connections it opens that are registered
// on an event loop (a direct connection is not: see [Connector.WithEngine]). A budget of one microsecond or more sets it, in
// whole microseconds, and wins over the server's; anything less (0, a negative
// value) leaves it to the server's Config.SocketBusyPoll when the Connector is
// bound with [Connector.WithEngine], else off, so it cannot turn off a server
// that has it on. Busy polling is off by default. Before v1.6.0 the driver set
// 50 microseconds on every connection of a Connector (celeris#1228). A refused
// setsockopt is ignored, silently. Like [Connector.WithEngine] it returns a new
// Connector that shares the DSN and not the loop.
func (c *Connector) WithSocketBusyPoll(d time.Duration) *Connector {
	clone := &Connector{dsn: c.dsn, engine: c.engine, busyPoll: d}
	c.provMu.Lock()
	if c.provider != nil && !c.ownsLoop {
		clone.provider = c.provider
	}
	c.provMu.Unlock()
	return clone
}

// Connect returns a new driver.Conn. database/sql's pool reuses this
// connector across all pooled conns, so we resolve the provider once and
// share it for the connector's lifetime.
//
// A connection is registered on an event loop, except in one case: the
// Connector took the loops of a started epoll, io_uring or adaptive server (they
// have no synchronous round trip for a driver). A handler of that server runs
// inline on one of those workers, and a handler that opens a connection would
// wait for the startup reply on the very worker that has to read it. Such a
// connection is dialled direct (Go's netpoll, like a Pool's), by the rule a
// [Pool] applies (loopLacksSyncPath). The provider is fixed when the Connector
// is bound ([Connector.WithEngine]); the rule is evaluated at each Connect
// against it, so a Connector bound before the server starts, or on the std
// engine, or without an engine, keeps its connections on the driver's own loop.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	prov, owns, err := c.ensureProvider()
	if err != nil {
		return nil, err
	}
	// The driver's own loop (an unstarted server, the std engine, no engine)
	// is not a worker of the server: nothing to deadlock on, and a connection
	// stays on the loop it was dialled on.
	if !owns && loopLacksSyncPath(prov) {
		dc, err := dialDirectConn(ctx, c.dsn)
		if err != nil {
			return nil, err // not dc: a nil *pgConn would be a non-nil driver.Conn
		}
		return dc, nil
	}
	// Each conn releases the loop on Close if and only if the connector
	// itself owns it — but we share one loop across all conns, so the
	// refcount lives here, not on the conn. The pgConn's closeLoop is nil.
	dsn := c.dsn
	dsn.Options.busyPoll = eventloop.BusyPoll(c.engine, c.busyPoll)
	return dialConn(ctx, prov, nil, dsn, -1)
}

// Driver returns the singleton Driver.
func (c *Connector) Driver() driver.Driver { return &Driver{} }

// Close releases the standalone event loop if this connector owns one.
// database/sql calls Close via DB.Close.
func (c *Connector) Close() error {
	c.provMu.Lock()
	prov := c.provider
	owns := c.ownsLoop
	c.provider = nil
	c.ownsLoop = false
	c.provMu.Unlock()
	if owns && prov != nil {
		eventloop.Release(prov)
	}
	return nil
}

// ensureProvider lazily resolves the event loop. Thread-safe.
func (c *Connector) ensureProvider() (engine.EventLoopProvider, bool, error) {
	c.provMu.Lock()
	defer c.provMu.Unlock()
	if c.provider != nil {
		return c.provider, c.ownsLoop, nil
	}
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		return nil, false, err
	}
	c.provider = prov
	c.ownsLoop = true
	return prov, true, nil
}
