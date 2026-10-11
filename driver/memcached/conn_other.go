//go:build !unix

package memcached

import (
	"context"

	"github.com/goceleris/celeris/internal/engine"
)

// dialMemcachedConn dials a TCP connection and registers the net.Conn itself
// with the event loop: a platform without unix file descriptors (windows) has
// no descriptor to extract, set non-blocking or duplicate. See
// dialMemcachedConnNetConn.
func dialMemcachedConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*memcachedConn, error) {
	return dialMemcachedConnNetConn(ctx, prov, cfg, workerHint)
}
