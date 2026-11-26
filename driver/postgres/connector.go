package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// Connector is a database/sql/driver.Connector that opens celeris-backed
// PostgreSQL connections. It implements driver.Connector and additionally
// offers a WithEngine method that rebinds the connector to a running
// celeris.Server's event loop.
//
// By default its connections are direct: each does its I/O on the calling
// goroutine through Go's netpoll, reads a reply whole before the call returns,
// and is not registered on an event loop (the connections of a [Pool] without
// [WithEngine]). [Connector.WithStreaming], or streaming=true in the DSN, puts
// them on the event loop instead.
type Connector struct {
	dsn DSN

	// streaming selects the event-loop connections (WithStreaming, or the
	// streaming=true DSN parameter); false, the default, dials direct ones.
	streaming bool

	// maxResultBytes is the result buffer cap set by WithMaxResultBytes, and
	// maxResultSet whether it was set: a set value below one byte is refused by
	// Connect, not taken for the default.
	maxResultBytes int
	maxResultSet   bool

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
	return &Connector{dsn: parsed, streaming: parsed.Options.streaming}, nil
}

// clone returns a copy of c that shares the DSN and not the loop: the loop is
// resolved again, or taken from the server, by the copy.
func (c *Connector) clone() *Connector {
	return &Connector{
		dsn: c.dsn, engine: c.engine, busyPoll: c.busyPoll, streaming: c.streaming,
		maxResultBytes: c.maxResultBytes, maxResultSet: c.maxResultSet,
	}
}

// WithEngine rebinds this Connector to sp's event loop. The returned
// Connector shares the DSN but replaces the provider; it does not own the
// loop (so Close does not tear it down).
//
// The loop is used by the connections of [Connector.WithStreaming] only: a
// Connector without it dials direct connections, which are not on a loop, and
// WithEngine then has no effect on them.
//
// The connections it opens on the loop take the server's Config.SocketBusyPoll
// as their SO_BUSY_POLL budget (Linux; 0, off, by default), unless
// [Connector.WithSocketBusyPoll] set one of its own.
//
// The driver takes the server's event loop once, when it is opened: open it after
// srv.Addr() is non-nil to share the server's loops; opened earlier it uses the
// driver's own loop for its whole life.
//
// With [Connector.WithStreaming] the mode is decided at each Connect:
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
// A direct connection (that one, and every connection of a Connector without
// WithStreaming) has the limits of a direct-mode Pool: a context that ends, by a
// deadline or a cancel, ends the statement, and the call returns the context's
// error (context.DeadlineExceeded, context.Canceled), not a *PGError with
// SQLSTATE 57014; a result is read whole and fails with [ErrResultTooBig] past
// the cap (64 MiB by default, see [Connector.WithMaxResultBytes]), so there is
// no row streaming; LISTEN and UNLISTEN fail with [ErrDirectModeUnsupported]; and
// [Connector.WithSocketBusyPoll] has no effect, as the connection is not on an
// event loop. The mode follows the server, not the caller: a background
// goroutine that uses the Connector gets the same connections. To keep
// streaming and the loop's behaviour with WithStreaming, bind the Connector
// before the server starts, or use no engine.
func (c *Connector) WithEngine(sp ServerProvider) *Connector {
	clone := c.clone()
	clone.engine = sp
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
// SO_BUSY_POLL budget (Linux) of the connections it registers on an event loop,
// those of [Connector.WithStreaming]; a direct connection, the default, is not on
// a loop and has no busy polling, with this option or the server's. A budget of
// one microsecond or more sets it, in whole microseconds, and wins over the
// server's; anything less (0, a negative value) leaves it to the server's
// Config.SocketBusyPoll when the Connector is bound with [Connector.WithEngine],
// else off, so it cannot turn off a server that has it on. Busy polling is off by
// default. Before v1.6.0 the driver set 50 microseconds on every connection of a
// Connector (celeris#1228). A refused setsockopt is ignored, silently. Like
// [Connector.WithEngine] it returns a new Connector that shares the DSN and not
// the loop.
func (c *Connector) WithSocketBusyPoll(d time.Duration) *Connector {
	clone := c.clone()
	clone.busyPoll = d
	c.provMu.Lock()
	if c.provider != nil && !c.ownsLoop {
		clone.provider = c.provider
	}
	c.provMu.Unlock()
	return clone
}

// WithStreaming returns a Connector that is this one with its connections
// registered on the event loop: the loop of the server given to
// [Connector.WithEngine], else the driver's own. The same is streaming=true in the
// DSN (an unknown value is an error). It returns a new Connector that shares the
// DSN and not the loop, like [Connector.WithEngine].
//
// The default, without it, is a direct connection: the I/O runs on the calling
// goroutine, a reply is read whole before the call returns, and the other callers
// do not wait for each other (see [Connector]). Streaming is what the loop gives:
// a result is delivered row by row as it arrives, the first row before the last
// one is read, in memory proportional to what the caller has not read, and with no
// result size cap ([Connector.WithMaxResultBytes] does not apply). Use it for a
// result too large to hold in memory, or to read the first rows of one early.
//
// One case cannot stream: a Connector bound with [Connector.WithEngine] to a
// server that has started on the epoll, io_uring or adaptive engine. Their workers
// run an inline handler on the worker's own thread and have no synchronous round
// trip for a driver, so a handler that opened a connection on the loop would wait
// for the startup reply on the very worker that has to read it, and the server
// would stop answering (it did before v1.6.0). Such a Connector dials DIRECT
// connections even with WithStreaming, with everything that goes with them: the
// results are read whole and capped ([Connector.WithMaxResultBytes] applies), and
// the limits below do not apply. The rule is evaluated at each connect against the
// loops the Connector took when it was bound, the one a [Pool] applies: a
// Connector bound before the server starts, one on the std engine, and one with no
// engine stay on the driver's own loop.
//
// The limits of the path it selects, which direct connections do not have:
//
//   - A streaming [database/sql.Rows] whose consumer stops reading, or reads
//     slowly, holds the loop worker that serves the connection, and with it every
//     other connection placed on that worker: their calls wait, up to their own
//     deadline or for ever.
//   - A query run inside a loop over the rows of another query ("for rows.Next()
//     { db.Query(...) }") can stall: the worker waits for the consumer to take a
//     row while the nested query's reply needs that same worker.
//   - A second statement on the same connection while a Rows is open (a
//     [database/sql.Tx] or [database/sql.Conn]) can block, and a context does not
//     end it.
//   - A context deadline that fires while a large result streams in can hang the
//     call.
//
// These are tracked in goceleris/celeris#1436, which also says what is decided
// about them. A program that cannot meet them should not use WithStreaming.
//
// LISTEN and UNLISTEN are accepted on this path, but notifications are not
// delivered: the driver has no way to receive them (the behaviour of v1.5.11), and
// a direct connection refuses the statement for that reason
// ([ErrDirectModeUnsupported]).
//
// Busy polling applies to this path only: [Connector.WithSocketBusyPoll], and the
// server's Config.SocketBusyPoll through [Connector.WithEngine], set SO_BUSY_POLL
// on the connections WithStreaming registers on the loop, and on no direct one.
func (c *Connector) WithStreaming() *Connector {
	clone := c.clone()
	clone.streaming = true
	c.provMu.Lock()
	if c.provider != nil && !c.ownsLoop {
		clone.provider = c.provider
	}
	c.provMu.Unlock()
	return clone
}

// WithMaxResultBytes returns a Connector that is this one with n as the result
// buffer cap of its direct connections: the most DataRow payload bytes a single
// statement's result may have. A direct connection reads a reply whole into
// memory, and a statement whose result is over the cap fails with
// [ErrResultTooBig] ([database/sql.Rows.Err] after the rows that fit). The default
// is 64 MiB. The cap bounds memory, so raising it is the caller's explicit
// choice: n must be at least 1; zero (or a negative value) is not "no cap", and
// [Connector.Connect] returns an error for it. There is no way to turn the cap
// off. Connections opened with [Connector.WithStreaming] stream and have no cap.
// Like [Connector.WithEngine] it returns a new Connector that shares the DSN and
// not the loop.
func (c *Connector) WithMaxResultBytes(n int) *Connector {
	clone := c.clone()
	clone.maxResultBytes, clone.maxResultSet = n, true
	// Keep the loops WithEngine took from a server, as WithStreaming and
	// WithSocketBusyPoll do: without them Connect would resolve the driver's own
	// loop and a Connector bound to a started server would stream on it instead of
	// dialling direct.
	c.provMu.Lock()
	if c.provider != nil && !c.ownsLoop {
		clone.provider = c.provider
	}
	c.provMu.Unlock()
	return clone
}

// Connect returns a new driver.Conn: a direct one, unless the Connector has
// [Connector.WithStreaming], which registers it on an event loop except when the
// Connector took the loops of a started epoll, io_uring or adaptive server (see
// [Connector.WithStreaming]; the rule is loopLacksSyncPath, shared with [Pool]).
// database/sql's pool reuses this connector across all pooled conns, so a
// streaming Connector resolves the provider once and shares it for the connector's
// lifetime.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	dsn := c.dsn
	if c.maxResultSet {
		if c.maxResultBytes < 1 {
			return nil, fmt.Errorf("celeris-postgres: WithMaxResultBytes(%d): the cap must be at least 1 byte (there is no way to turn it off)", c.maxResultBytes)
		}
	}
	if c.streaming {
		prov, owns, err := c.ensureProvider()
		if err != nil {
			return nil, err
		}
		// The driver's own loop (an unstarted server, the std engine, no engine)
		// is not a worker of the server: nothing to deadlock on, and a connection
		// stays on the loop it was dialled on. A started epoll, io_uring or
		// adaptive server's workers run an inline handler on their own thread and
		// have no synchronous round trip: that case is dialled direct below.
		if owns || !loopLacksSyncPath(prov) {
			// Each conn releases the loop on Close if and only if the connector
			// itself owns it — but we share one loop across all conns, so the
			// refcount lives here, not on the conn. The pgConn's closeLoop is nil.
			dsn.Options.busyPoll = eventloop.BusyPoll(c.engine, c.busyPoll)
			return dialConn(ctx, prov, nil, dsn, -1)
		}
	}
	// The default, and a streaming Connector on a started inline server: the dial a
	// direct-mode Pool does (Pool.dial). No loop of the driver's own is resolved
	// for the default, and there is no busy-poll budget to set.
	dsn.Options.maxResultBytes = c.maxResultBytes
	dc, err := dialDirectConn(ctx, dsn)
	if err != nil {
		return nil, err // not dc: a nil *pgConn would be a non-nil driver.Conn
	}
	return dc, nil
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
