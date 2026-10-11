//go:build !unix

package redis

import (
	"context"

	"github.com/goceleris/celeris/internal/engine"
)

// dialRedisConn dials a TCP connection and registers the net.Conn itself with
// the event loop: a platform without unix file descriptors (windows) has no
// descriptor to extract, set non-blocking or duplicate. See
// dialRedisConnNetConn.
func dialRedisConn(ctx context.Context, prov engine.EventLoopProvider, cfg Config, workerHint int) (*redisConn, error) {
	return dialRedisConnNetConn(ctx, prov, cfg, workerHint)
}
