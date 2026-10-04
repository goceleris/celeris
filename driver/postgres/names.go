package postgres

import (
	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// TypeCodec encodes and decodes a single PostgreSQL type, identified by its
// OID. Register one with [RegisterType] to teach the driver a type it does
// not know, or to replace a built-in codec.
//
// The fields are OID, Name, DecodeText, DecodeBinary, EncodeText,
// EncodeBinary and ScanType:
//
//   - OID uint32 and Name string identify the type.
//   - DecodeText and DecodeBinary, func(src []byte) (driver.Value, error),
//     decode a column value in the text or binary wire format.
//   - EncodeText and EncodeBinary, func(dst []byte, v any) ([]byte, error),
//     follow the append convention: they append the encoded bytes to dst
//     and return the (possibly reallocated) slice. An encoder that receives
//     a driver.Valuer calls Value() first and dispatches on the result. A
//     codec never sees nil; the driver writes NULL itself.
//   - ScanType reflect.Type is what [database/sql.ColumnType.ScanType]
//     reports for the column.
type TypeCodec = protocol.TypeCodec

// RegisterType registers a custom type codec. Later registrations override
// earlier ones for the same OID. It is safe to call at init time or at
// run time. RegisterType panics if c is nil.
//
// The registry keeps c itself, and the driver calls it from every
// goroutine that encodes or decodes a value of that OID. Do not modify c
// after registering it: register a new codec instead.
func RegisterType(c *TypeCodec) { protocol.RegisterType(c) }

// LookupOID returns the codec registered for oid, or nil if none is
// registered.
//
// The result is the registered codec itself, shared with the driver, which
// calls it from every goroutine that encodes or decodes a value of that
// OID. Do not modify it. To change a codec, for example to wrap its
// DecodeText, copy the struct, change the copy and pass the copy to
// [RegisterType].
func LookupOID(oid uint32) *TypeCodec { return protocol.LookupOID(oid) }

// PoolStats reports the occupancy of a [Pool]'s connections, as returned
// by [Pool.Stats].
//
// The fields are Open, the number of connections, idle and in use; Idle,
// the number on the workers' idle lists; InUse, Open minus Idle; and
// PerWorker, a []PoolWorkerStats with one entry per event-loop worker.
type PoolStats = async.PoolStats

// PoolWorkerStats is the per-worker part of [PoolStats]. Its one field,
// Idle int, is the number of idle connections on that worker's list.
type PoolWorkerStats = async.PoolWorkerStats

// ServerProvider is what [WithEngine] and [Connector.WithEngine] take.
// *celeris.Server implements it; pass the server to share its event loop.
// Its one method returns a type defined in an internal package, so in
// practice *celeris.Server is the only implementation. That type is not
// supported API until celeris#453; see
// [github.com/goceleris/celeris.Server.EventLoopProvider].
type ServerProvider = eventloop.ServerProvider

// ErrQueueFull is returned when the event-loop worker that owns a
// connection has no room left in that connection's outbound queue. The
// message was not sent. Match it with errors.Is.
var ErrQueueFull = engine.ErrQueueFull

// ErrUnknownFD is returned when an event-loop operation refers to a
// connection that is no longer registered on its worker, for example after
// the worker tore the connection down. Match it with errors.Is.
var ErrUnknownFD = engine.ErrUnknownFD

// ErrInvalidLength is returned when the server sends a message whose length
// header is below the 4-byte minimum or above the driver's 1 GiB limit. The
// connection is closed. Match it with errors.Is.
var ErrInvalidLength = protocol.ErrInvalidLength

// PGInfinity and PGNegInfinity are the time.Time values the driver returns
// when PostgreSQL sends infinity or -infinity for a date, timestamp or
// timestamptz column: 9999-12-31T23:59:59Z and -4713-01-01T00:00:00Z.
// Check for them with time.Equal before doing arithmetic on the value.
var (
	PGInfinity    = protocol.PGInfinity
	PGNegInfinity = protocol.PGNegInfinity
)
