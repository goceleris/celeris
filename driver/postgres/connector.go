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
	// busyPoll when positive (WithSocketBusyPoll), else the server's
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
// [Connector.WithSocketBusyPoll] set one.
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
// SO_BUSY_POLL budget (Linux) of the connections it opens: a positive d sets
// it, whole microseconds only. Busy polling is off by default, and the budget
// of a Connector bound with [Connector.WithEngine] is the server's
// Config.SocketBusyPoll unless a positive d overrides it, which it does; it
// is the only way to enable it without an engine. A zero d inherits and so
// cannot turn off a server that has it on. Before v1.6.0
// the driver set 50 microseconds on every connection with no way to turn it
// off (celeris#1228). Like [Connector.WithEngine] it returns a new Connector
// that shares the DSN and not the loop.
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
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	prov, owns, err := c.ensureProvider()
	if err != nil {
		return nil, err
	}
	_ = owns
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
