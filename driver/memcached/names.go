package memcached

import (
	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/engine"
)

// PoolStats reports the occupancy of a client's connection pool, as
// returned by [Client.PoolStats].
type PoolStats = async.PoolStats

// PoolWorkerStats is the per-worker part of [PoolStats].
type PoolWorkerStats = async.PoolWorkerStats

// ServerProvider is what [WithEngine] and the Engine fields of [Config] and
// [ClusterConfig] take. *celeris.Server implements it; pass the server to
// share its event loop.
type ServerProvider = eventloop.ServerProvider

// ErrQueueFull is returned when the event-loop worker that owns a
// connection has no room left in that connection's outbound queue. The
// command was not sent. Match it with errors.Is.
var ErrQueueFull = engine.ErrQueueFull

// ErrUnknownFD is returned when an event-loop operation refers to a
// connection that is no longer registered on its worker, for example after
// the worker tore the connection down. Match it with errors.Is.
var ErrUnknownFD = engine.ErrUnknownFD
