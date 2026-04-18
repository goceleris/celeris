package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// CopyFromSource is the interface consumed by Pool.CopyFrom. It is modeled
// after pgx.CopyFromSource: Next advances one row at a time, Values returns
// the current row's column values, and Err surfaces any iteration error.
//
// Implementations are driven synchronously on the caller's goroutine; no
// concurrent access is expected. Returned slices may be reused between
// iterations — the driver consumes them before calling Next again.
type CopyFromSource interface {
	Next() bool
	Values() ([]any, error)
	Err() error
}

// CopyFromSlice wraps a [][]any fixture as a CopyFromSource. Useful for
// tests and for callers that already have their rows materialised in
// memory.
func CopyFromSlice(rows [][]any) CopyFromSource {
	return &sliceCopySource{rows: rows, idx: -1}
}

type sliceCopySource struct {
	rows [][]any
	idx  int
}

func (s *sliceCopySource) Next() bool {
	s.idx++
	return s.idx < len(s.rows)
}

func (s *sliceCopySource) Values() ([]any, error) {
	if s.idx < 0 || s.idx >= len(s.rows) {
		return nil, fmt.Errorf("celeris-postgres: CopyFromSlice out of range (idx=%d)", s.idx)
	}
	return s.rows[s.idx], nil
}

func (s *sliceCopySource) Err() error { return nil }

// encodeTextRow appends the PG COPY-text encoding of the given values to
// dst and returns the extended slice. Columns are tab-separated; the row
// is terminated by '\n'. NULL is encoded as `\N`. Control characters and
// special escapes (`\b`, `\f`, `\n`, `\r`, `\t`, `\v`, `\\`) are
// backslash-escaped per PG's COPY text format.
//
// Supported value types: nil, bool, string, []byte, int/int8/int16/int32/
// int64, uint/uint8/uint16/uint32/uint64, float32, float64, time.Time, and
// fmt.Stringer. Anything else is rendered via fmt.Sprintf("%v", v).
func encodeTextRow(dst []byte, values []any) []byte {
	for i, v := range values {
		if i > 0 {
			dst = append(dst, '\t')
		}
		dst = appendTextField(dst, v)
	}
	dst = append(dst, '\n')
	return dst
}

// appendTextField renders one value into the PG COPY text format.
func appendTextField(dst []byte, v any) []byte {
	if v == nil {
		return append(dst, '\\', 'N')
	}
	switch x := v.(type) {
	case bool:
		if x {
			return append(dst, 't')
		}
		return append(dst, 'f')
	case string:
		return appendEscapedString(dst, x)
	case []byte:
		return appendEscapedBytes(dst, x)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case int8:
		return strconv.AppendInt(dst, int64(x), 10)
	case int16:
		return strconv.AppendInt(dst, int64(x), 10)
	case int32:
		return strconv.AppendInt(dst, int64(x), 10)
	case int64:
		return strconv.AppendInt(dst, x, 10)
	case uint:
		return strconv.AppendUint(dst, uint64(x), 10)
	case uint8:
		return strconv.AppendUint(dst, uint64(x), 10)
	case uint16:
		return strconv.AppendUint(dst, uint64(x), 10)
	case uint32:
		return strconv.AppendUint(dst, uint64(x), 10)
	case uint64:
		return strconv.AppendUint(dst, x, 10)
	case float32:
		return strconv.AppendFloat(dst, float64(x), 'g', -1, 32)
	case float64:
		return strconv.AppendFloat(dst, x, 'g', -1, 64)
	case time.Time:
		// RFC3339Nano is unambiguous and PG's timestamp parser accepts it.
		return appendEscapedString(dst, x.UTC().Format(time.RFC3339Nano))
	case fmt.Stringer:
		return appendEscapedString(dst, x.String())
	default:
		return appendEscapedString(dst, fmt.Sprintf("%v", v))
	}
}

// appendEscapedString appends s with PG's COPY text escapes applied.
func appendEscapedString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		dst = appendEscapedByte(dst, s[i])
	}
	return dst
}

func appendEscapedBytes(dst []byte, b []byte) []byte {
	for i := 0; i < len(b); i++ {
		dst = appendEscapedByte(dst, b[i])
	}
	return dst
}

// appendEscapedByte writes one byte with PG COPY-text escapes applied.
// Special sequences:
//
//	\b   -> \b    (0x08)
//	\f   -> \f    (0x0c)
//	\n   -> \n    (0x0a)
//	\r   -> \r    (0x0d)
//	\t   -> \t    (0x09)
//	\v   -> \v    (0x0b)
//	\\   -> \\
//
// Other control characters (< 0x20 or 0x7f) are kept verbatim: PG accepts
// them. We explicitly *do not* escape tabs unless they appear inside a
// field, because the separator is itself a tab — therefore we always
// escape them here to avoid corrupting the frame.
func appendEscapedByte(dst []byte, b byte) []byte {
	switch b {
	case '\\':
		return append(dst, '\\', '\\')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	case '\v':
		return append(dst, '\\', 'v')
	default:
		return append(dst, b)
	}
}

// CopyFrom streams rows from src to the server via COPY FROM STDIN. Returns
// the number of rows imported (parsed from the server's CommandComplete tag)
// or any transport / iteration error. Column names may be empty to default
// to the table's natural column order.
func (p *Pool) CopyFrom(ctx context.Context, tableName string, columns []string, src CopyFromSource) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return c.copyFrom(ctx, tableName, columns, src)
}

// CopyTo executes query (typically "COPY <table> TO STDOUT ...") and invokes
// dest for each row. The dest slice is freshly allocated per call, so
// callers are free to retain it without copying. Aborting mid-stream by
// returning a non-nil error from dest terminates the copy — the server
// continues to stream bytes until ReadyForQuery, so the conn remains usable
// on return.
func (p *Pool) CopyTo(ctx context.Context, query string, dest func(row []byte) error) error {
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return c.copyTo(ctx, query, dest)
}
