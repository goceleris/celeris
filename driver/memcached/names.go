package memcached

import (
	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/engine"
)

// PoolStats reports the occupancy of a client's connection pool, as
// returned by [Client.PoolStats].
//
// The fields are Open, the number of connections, idle and in use; Idle,
// the number on the workers' idle lists; InUse, Open minus Idle; and
// PerWorker, a []PoolWorkerStats with one entry per event-loop worker.
type PoolStats = async.PoolStats

// PoolWorkerStats is the per-worker part of [PoolStats]. Its one field,
// Idle int, is the number of idle connections on that worker's list.
type PoolWorkerStats = async.PoolWorkerStats

// ServerProvider is what [WithEngine] and the Engine fields of [Config] and
// [ClusterConfig] take. *celeris.Server implements it; pass the server to
// share its event loop.
// Its one method returns a type defined in an internal package, so in
// practice *celeris.Server is the only implementation. That type is not
// supported API until celeris#453; see
// [github.com/goceleris/celeris.Server.EventLoopProvider].
type ServerProvider = eventloop.ServerProvider

// ErrQueueFull is returned when the event-loop worker that owns a
// connection has no room left in that connection's outbound queue. The
// command was not sent. Match it with errors.Is.
var ErrQueueFull = engine.ErrQueueFull

// ErrUnknownFD is returned when an event-loop operation refers to a
// connection that is no longer registered on its worker, for example after
// the worker tore the connection down. Match it with errors.Is.
var ErrUnknownFD = engine.ErrUnknownFD
