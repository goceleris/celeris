package postgres

import (
	"database/sql/driver"
	"io"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// pgRows wraps the row buffer accumulated by the state machine. It iterates
// by decoding each field with the TypeCodec registered for its OID.
type pgRows struct {
	columns []protocol.ColumnDesc
	// codecs is populated once in acquirePGRows: codecs[i] is the cached
	// TypeCodec for column i (may be nil if the OID is unknown). Resolving
	// once per query instead of once per cell saves an RWMutex RLock per
	// row×column under tight iteration.
	codecs []*protocol.TypeCodec
	rows   [][][]byte
	idx    int
	// textFormat is true for simple-query results (always text encoding) and
	// false for extended-query results that requested binary.
	textFormat bool
	closed     bool
	// err is a deferred error from the server (e.g. ErrorResponse mid-stream)
	// that is surfaced after all buffered rows have been consumed via Next().
	err error
	// req (if non-nil) is the pooled pgRequest that owns the backing slabs
	// for columns/rows/rowFields/rowSlab. We return it to the pool when the
	// caller is done iterating (Close), so the slab memory recycles rather
	// than leaking alloc pressure into every QueryContext call.
	req *pgRequest
	// codecsBuf is the inline backing storage for small column counts
	// (<=16). Larger queries fall back to a heap-allocated slice.
	codecsBuf [16]*protocol.TypeCodec
}

var (
	_ driver.Rows                           = (*pgRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*pgRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*pgRows)(nil)
)

// pgRowsPool amortizes the pgRows struct allocation across queries.
var pgRowsPool = sync.Pool{
	New: func() any { return &pgRows{} },
}

// acquirePGRows returns a pgRows initialized from the given request. The
// caller surrenders ownership of req to the pgRows; Close releases both.
// If deferredErr is non-nil, it is surfaced after all buffered rows are
// consumed (e.g. mid-stream ErrorResponse that arrived with partial data).
func acquirePGRows(cols []protocol.ColumnDesc, rows [][][]byte, textFormat bool, req *pgRequest, deferredErr error) *pgRows {
	r := pgRowsPool.Get().(*pgRows)
	r.columns = cols
	r.rows = rows
	r.idx = 0
	r.textFormat = textFormat
	r.closed = false
	r.err = deferredErr
	r.req = req
	// Resolve one codec per column up front. Hits the RWMutex once per
	// column instead of once per cell.
	if len(cols) <= len(r.codecsBuf) {
		r.codecs = r.codecsBuf[:len(cols)]
	} else {
		r.codecs = make([]*protocol.TypeCodec, len(cols))
	}
	for i, c := range cols {
		r.codecs[i] = protocol.LookupOID(c.TypeOID)
	}
	return r
}

func (r *pgRows) Columns() []string {
	out := make([]string, len(r.columns))
	for i, c := range r.columns {
		out[i] = c.Name
	}
	return out
}

func (r *pgRows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.rows = nil
	r.columns = nil
	r.err = nil
	req := r.req
	r.req = nil
	if req != nil {
		releasePgRequest(req)
	}
	// Return the pgRows struct itself to the pool. idx/textFormat are
	// overwritten on the next Get.
	pgRowsPool.Put(r)
	return nil
}

// Next decodes the next row into dest. Returns io.EOF at end, or a deferred
// error if one was recorded (e.g. mid-stream ErrorResponse).
func (r *pgRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		if r.err != nil {
			return r.err
		}
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	for i := range dest {
		if i >= len(row) {
			dest[i] = nil
			continue
		}
		raw := row[i]
		if raw == nil {
			dest[i] = nil
			continue
		}
		col := r.columns[i]
		codec := r.codecs[i]
		if codec == nil {
			// Unknown type — return raw bytes.
			cp := make([]byte, len(raw))
			copy(cp, raw)
			dest[i] = cp
			continue
		}
		var (
			v   driver.Value
			err error
		)
		if r.textFormat || col.FormatCode == protocol.FormatText {
			if codec.DecodeText != nil {
				v, err = codec.DecodeText(raw)
			} else {
				cp := make([]byte, len(raw))
				copy(cp, raw)
				v = cp
			}
		} else {
			if codec.DecodeBinary != nil {
				v, err = codec.DecodeBinary(raw)
			} else if codec.DecodeText != nil {
				v, err = codec.DecodeText(raw)
			} else {
				cp := make([]byte, len(raw))
				copy(cp, raw)
				v = cp
			}
		}
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

// ColumnTypeScanType returns the Go type the i-th column will decode into.
func (r *pgRows) ColumnTypeScanType(i int) reflect.Type {
	if i < 0 || i >= len(r.columns) {
		return reflect.TypeOf([]byte(nil))
	}
	codec := protocol.LookupOID(r.columns[i].TypeOID)
	if codec == nil || codec.ScanType == nil {
		return reflect.TypeOf([]byte(nil))
	}
	return codec.ScanType
}

// ColumnTypeDatabaseTypeName returns the PG type name (or "UNKNOWN") for the
// i-th column.
func (r *pgRows) ColumnTypeDatabaseTypeName(i int) string {
	if i < 0 || i >= len(r.columns) {
		return ""
	}
	codec := protocol.LookupOID(r.columns[i].TypeOID)
	if codec == nil {
		return "UNKNOWN"
	}
	return codec.Name
}

// HasNextResultSet reports whether a multi-statement simple query produced
// another result set after the current one. For v1.4.0 we flatten multi-
// statement results into a single pgRows, so this is always false.
func (r *pgRows) HasNextResultSet() bool { return false }

// NextResultSet advances to the next result set; always returns io.EOF for
// now.
func (r *pgRows) NextResultSet() error { return io.EOF }

// ---------------------------------------------------------------------------
// streamRows — streaming row iterator
// ---------------------------------------------------------------------------

// streamRowsChanSize is the bounded channel capacity for streamed rows.
// When the channel is full, the event loop's dispatch blocks on send, which
// pauses reading from the PG socket and lets TCP flow control back-pressure
// the server. 64 rows is enough to amortize channel overhead while capping
// memory to ~64 * avg_row_size.
const streamRowsChanSize = 64

// streamThreshold is the number of buffered rows at which dispatch switches
// from slab-buffered mode to channel-streaming mode. Queries returning fewer
// than this many rows never allocate a channel -- eliminating the ~2KB
// make(chan [][]byte, 64) allocation that dominated small-query benchmarks.
// The threshold equals streamRowsChanSize so all buffered rows fit in the
// channel without blocking when the transition occurs.
const streamThreshold = streamRowsChanSize

// streamRows implements driver.Rows with a pull-based channel interface.
// Instead of buffering all rows before returning to the caller, the dispatch
// path sends each DataRow to rowCh. The caller's Next() reads one row at a
// time. Back-pressure propagates through the bounded channel to the PG
// socket via TCP flow control.
//
// Lifecycle: simpleQuery / doExtendedQuery create a streamRows with the
// RowDescription columns, hand it the rowCh, and return it immediately.
// dispatch sends each DataRow (copied into owned memory) to rowCh.
// On CommandComplete / ErrorResponse / ReadyForQuery, dispatch closes rowCh
// and signals doneCh. Close() drains any remaining rows and waits for doneCh.
type streamRows struct {
	columns    []protocol.ColumnDesc
	codecs     []*protocol.TypeCodec // cached lookup per column
	textFormat bool

	rowCh  chan [][]byte          // bounded channel; closed by dispatch on completion
	doneCh chan struct{}          // signaled after ReadyForQuery (request fully done)
	req    *pgRequest             // owns slab memory; released on Close
	errVal *atomic.Pointer[error] // points to req.streamErr; set before close(rowCh)

	current [][]byte // last row received from rowCh
	err     error    // cached error from errVal
	closed  atomic.Bool
}

var (
	_ driver.Rows                           = (*streamRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*streamRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*streamRows)(nil)
)

func (r *streamRows) Columns() []string {
	out := make([]string, len(r.columns))
	for i, c := range r.columns {
		out[i] = c.Name
	}
	return out
}

// Next reads the next row from the channel. Returns io.EOF when all rows
// have been consumed (channel closed by dispatch).
func (r *streamRows) Next(dest []driver.Value) error {
	row, ok := <-r.rowCh
	if !ok {
		// Channel closed — check for server error. The error was stored
		// atomically before close(rowCh), so the load is race-free.
		if ep := r.errVal.Load(); ep != nil && *ep != nil {
			r.err = *ep
			return r.err
		}
		return io.EOF
	}
	r.current = row
	for i := range dest {
		if i >= len(row) {
			dest[i] = nil
			continue
		}
		raw := row[i]
		if raw == nil {
			dest[i] = nil
			continue
		}
		col := r.columns[i]
		var codec *protocol.TypeCodec
		if i < len(r.codecs) {
			codec = r.codecs[i]
		}
		if codec == nil {
			cp := make([]byte, len(raw))
			copy(cp, raw)
			dest[i] = cp
			continue
		}
		var (
			v   driver.Value
			err error
		)
		if r.textFormat || col.FormatCode == protocol.FormatText {
			if codec.DecodeText != nil {
				v, err = codec.DecodeText(raw)
			} else {
				cp := make([]byte, len(raw))
				copy(cp, raw)
				v = cp
			}
		} else {
			if codec.DecodeBinary != nil {
				v, err = codec.DecodeBinary(raw)
			} else if codec.DecodeText != nil {
				v, err = codec.DecodeText(raw)
			} else {
				cp := make([]byte, len(raw))
				copy(cp, raw)
				v = cp
			}
		}
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

// Close drains any remaining rows from the channel, waits for the request
// to fully complete (ReadyForQuery), and releases the backing pgRequest.
func (r *streamRows) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Drain remaining rows so dispatch doesn't block forever.
	for range r.rowCh { //revive:disable-line:empty-block drain until closed
	}
	// Wait for the full request completion (ReadyForQuery).
	<-r.doneCh
	if ep := r.errVal.Load(); ep != nil && *ep != nil && r.err == nil {
		r.err = *ep
	}
	req := r.req
	r.req = nil
	r.columns = nil
	if req != nil {
		releasePgRequest(req)
	}
	return nil
}

// ColumnTypeScanType returns the Go type the i-th column will decode into.
func (r *streamRows) ColumnTypeScanType(i int) reflect.Type {
	if i < 0 || i >= len(r.columns) {
		return reflect.TypeOf([]byte(nil))
	}
	codec := protocol.LookupOID(r.columns[i].TypeOID)
	if codec == nil || codec.ScanType == nil {
		return reflect.TypeOf([]byte(nil))
	}
	return codec.ScanType
}

// ColumnTypeDatabaseTypeName returns the PG type name (or "UNKNOWN") for the
// i-th column.
func (r *streamRows) ColumnTypeDatabaseTypeName(i int) string {
	if i < 0 || i >= len(r.columns) {
		return ""
	}
	codec := protocol.LookupOID(r.columns[i].TypeOID)
	if codec == nil {
		return "UNKNOWN"
	}
	return codec.Name
}

func (r *streamRows) HasNextResultSet() bool { return false }
func (r *streamRows) NextResultSet() error   { return io.EOF }
