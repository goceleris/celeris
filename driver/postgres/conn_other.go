//go:build !unix

package postgres

import (
	"context"

	"github.com/goceleris/celeris/internal/engine"
)

// dialConn dials a TCP connection to the DSN's host:port and registers the
// net.Conn itself with the event loop: a platform without unix file
// descriptors (windows) has no descriptor to extract, set non-blocking or
// duplicate. See dialConnNetConn.
func dialConn(ctx context.Context, prov engine.EventLoopProvider, closeLoop func(), dsn DSN, workerHint int) (*pgConn, error) {
	return dialConnNetConn(ctx, prov, closeLoop, dsn, workerHint)
}
