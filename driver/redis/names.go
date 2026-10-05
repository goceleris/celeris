package redis

import (
	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/redis/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// Value is one decoded RESP2/RESP3 reply. [Client.Do], [Client.Eval],
// [Client.EvalSHA], [ClusterClient.Do], [SentinelClient.Do] and
// [ClusterPipeline.Exec] return it, and push callbacks ([WithOnPush],
// [Client.OnPush]) receive it.
//
// The fields are Type, Str, Int, Float, Bool, BigN, Array and Map. Type
// says which one carries the payload:
//
//   - [TySimple], [TyError], [TyBulk], [TyVerbatim], [TyBlobErr]: Str.
//   - [TyInt]: Int.
//   - [TyDouble]: Float.
//   - [TyBool]: Bool.
//   - [TyBigInt]: BigN (the raw ASCII digits).
//   - [TyArray], [TySet], [TyPush]: Array.
//   - [TyMap], [TyAttr]: Map, a slice of [KV].
//   - [TyNull]: none.
type Value = protocol.Value

// KV is one key/value pair (fields K and V, both [Value]) of a RESP3 map
// or attribute reply.
type KV = protocol.KV

// Type tags a [Value] with the RESP type of the reply.
type Type = protocol.Type

// The RESP types a [Value] can carry.
const (
	// TySimple is a RESP2/RESP3 simple string prefixed by '+'.
	TySimple = protocol.TySimple
	// TyError is a RESP2/RESP3 simple error prefixed by '-'.
	TyError = protocol.TyError
	// TyInt is a RESP2/RESP3 integer prefixed by ':'.
	TyInt = protocol.TyInt
	// TyBulk is a RESP2/RESP3 bulk string prefixed by '$'.
	TyBulk = protocol.TyBulk
	// TyArray is a RESP2/RESP3 array prefixed by '*'.
	TyArray = protocol.TyArray
	// TyNull is a RESP3 null ('_') or a RESP2 null-bulk/null-array ($-1/*-1).
	TyNull = protocol.TyNull
	// TyBool is a RESP3 boolean prefixed by '#'.
	TyBool = protocol.TyBool
	// TyDouble is a RESP3 double prefixed by ','.
	TyDouble = protocol.TyDouble
	// TyBigInt is a RESP3 big number prefixed by '('.
	TyBigInt = protocol.TyBigInt
	// TyBlobErr is a RESP3 blob error prefixed by '!'.
	TyBlobErr = protocol.TyBlobErr
	// TyVerbatim is a RESP3 verbatim string prefixed by '='.
	TyVerbatim = protocol.TyVerbatim
	// TySet is a RESP3 set prefixed by '~'.
	TySet = protocol.TySet
	// TyMap is a RESP3 map prefixed by '%'.
	TyMap = protocol.TyMap
	// TyAttr is a RESP3 attribute map prefixed by '|'.
	TyAttr = protocol.TyAttr
	// TyPush is a RESP3 push frame prefixed by '>'.
	TyPush = protocol.TyPush
)

// PoolStats reports the occupancy of a client's connection pool, as
// returned by [Client.Stats] and [SentinelClient.Stats].
type PoolStats = async.PoolStats

// PoolWorkerStats is the per-worker part of [PoolStats].
type PoolWorkerStats = async.PoolWorkerStats

// ServerProvider is what [WithEngine] and the Engine fields of [Config],
// [ClusterConfig] and [SentinelConfig] take. *celeris.Server implements it;
// pass the server to share its event loop.
type ServerProvider = eventloop.ServerProvider

// ErrQueueFull is returned when the event-loop worker that owns a
// connection has no room left in that connection's outbound queue. The
// command was not sent. Match it with errors.Is.
var ErrQueueFull = engine.ErrQueueFull

// ErrUnknownFD is returned when an event-loop operation refers to a
// connection that is no longer registered on its worker, for example after
// the worker tore the connection down. Match it with errors.Is.
var ErrUnknownFD = engine.ErrUnknownFD

// ErrProtocolOversizedBulk is returned when a reply advertises a bulk,
// verbatim or blob-error length above 512 MiB. The connection is closed.
// Match it with errors.Is.
var ErrProtocolOversizedBulk = protocol.ErrProtocolOversizedBulk

// ErrProtocolOversizedArray is returned when a reply advertises an array,
// set, push, map or attribute with more than 128Mi elements. The connection
// is closed. Match it with errors.Is.
var ErrProtocolOversizedArray = protocol.ErrProtocolOversizedArray
