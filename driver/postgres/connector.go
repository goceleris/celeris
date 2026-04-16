package postgres

import (
	"context"
	"database/sql/driver"
	"sync"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/engine"
)

// Connector is a database/sql/driver.Connector that opens celeris-backed
// PostgreSQL connections. It implements driver.Connector and additionally
// offers a WithEngine method that rebinds the connector to a running
// celeris.Server's event loop.
type Connector struct {
	dsn DSN

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
	return &Connector{dsn: parsed}, nil
}

// WithEngine rebinds this Connector to sp's event loop. The returned
// Connector shares the DSN but replaces the provider; it does not own the
// loop (so Close does not tear it down).
func (c *Connector) WithEngine(sp eventloop.ServerProvider) *Connector {
	clone := &Connector{dsn: c.dsn}
	if sp != nil {
		if p := sp.EventLoopProvider(); p != nil {
			clone.provider = p
			clone.ownsLoop = false
			return clone
		}
	}
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
	return dialConn(ctx, prov, nil, c.dsn, -1)
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
