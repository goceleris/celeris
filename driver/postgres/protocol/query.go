package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PGError is a server-sent ErrorResponse. The well-known fields (S, C, M,
// D, H, P) are exposed as typed members; everything else is captured in
// Extra for flexibility.
type PGError struct {
	Severity string
	Code     string // SQLSTATE
	Message  string
	Detail   string
	Hint     string
	Position int // 1-based character position in the query, 0 if absent

	Extra map[byte]string
}

// Error implements the error interface.
func (e *PGError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	if e.Severity != "" {
		b.WriteString(e.Severity)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.Code != "" {
		b.WriteString(" (SQLSTATE ")
		b.WriteString(e.Code)
		b.WriteString(")")
	}
	return b.String()
}

// ParseErrorResponse parses an ErrorResponse or NoticeResponse payload.
// Each field is a single-byte code followed by a CString value; the list is
// terminated by a zero byte.
func ParseErrorResponse(payload []byte) *PGError {
	e := &PGError{Extra: map[byte]string{}}
	pos := 0
	for pos < len(payload) {
		code := payload[pos]
		pos++
		if code == 0 {
			break
		}
		val, err := ReadCString(payload, &pos)
		if err != nil {
			break
		}
		switch code {
		case 'S':
			e.Severity = val
		case 'V':
			// non-localized severity (PG >= 9.6); keep in Extra too.
			e.Extra[code] = val
		case 'C':
			e.Code = val
		case 'M':
			e.Message = val
		case 'D':
			e.Detail = val
		case 'H':
			e.Hint = val
		case 'P':
			if n, err := strconv.Atoi(val); err == nil {
				e.Position = n
			}
			e.Extra[code] = val
		default:
			e.Extra[code] = val
		}
	}
	return e
}

// ColumnDesc describes a single column in a RowDescription.
type ColumnDesc struct {
	Name         string
	TableOID     uint32
	ColumnAttNum int16
	TypeOID      uint32
	TypeSize     int16
	TypeModifier int32
	FormatCode   int16 // 0=text, 1=binary
}

// ParseRowDescription parses a 'T' payload into a slice of ColumnDesc.
func ParseRowDescription(payload []byte) ([]ColumnDesc, error) {
	return ParseRowDescriptionInto(nil, payload)
}

// ParseRowDescriptionInto parses a 'T' payload into dst, reusing dst's
// backing array if large enough. Returns the (possibly reallocated)
// result slice. Passing dst=nil behaves like ParseRowDescription.
func ParseRowDescriptionInto(dst []ColumnDesc, payload []byte) ([]ColumnDesc, error) {
	if len(payload) < 2 {
		return nil, errors.New("postgres/protocol: short RowDescription")
	}
	pos := 0
	n, err := ReadInt16(payload, &pos)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("postgres/protocol: negative column count %d", n)
	}
	// PostgreSQL caps tuples at 1600 columns (MaxHeapAttributeNumber).
	// Rejecting anything above avoids a server-supplied int16 (up to
	// 32767) forcing a multi-MB allocation per RowDescription, and
	// matches the server's own limit.
	if n > 1600 {
		return nil, fmt.Errorf("postgres/protocol: column count %d exceeds MaxHeapAttributeNumber (1600)", n)
	}
	var cols []ColumnDesc
	if cap(dst) >= int(n) {
		cols = dst[:n]
		// Zero out any pre-existing name strings so we don't retain
		// references to old data beyond the new column count.
		for i := range cols {
			cols[i] = ColumnDesc{}
		}
	} else {
		cols = make([]ColumnDesc, n)
	}
	for i := range cols {
		name, err := ReadCString(payload, &pos)
		if err != nil {
			return nil, err
		}
		if pos+18 > len(payload) {
			return nil, errors.New("postgres/protocol: short column entry")
		}
		cols[i] = ColumnDesc{
			Name:         name,
			TableOID:     binary.BigEndian.Uint32(payload[pos:]),
			ColumnAttNum: int16(binary.BigEndian.Uint16(payload[pos+4:])),
			TypeOID:      binary.BigEndian.Uint32(payload[pos+6:]),
			TypeSize:     int16(binary.BigEndian.Uint16(payload[pos+10:])),
			TypeModifier: int32(binary.BigEndian.Uint32(payload[pos+12:])),
			FormatCode:   int16(binary.BigEndian.Uint16(payload[pos+16:])),
		}
		pos += 18
	}
	return cols, nil
}

// ParseDataRow returns a slice of field slices; nil indicates SQL NULL.
// Non-nil slices ALIAS payload — callers that retain them must copy.
func ParseDataRow(payload []byte) ([][]byte, error) {
	return ParseDataRowInto(nil, payload)
}

// ParseDataRowInto parses a 'D' payload into dst, reusing dst's backing
// array if large enough. Returns the (possibly reallocated) result slice.
// Passing dst=nil behaves like ParseDataRow. Non-nil field slices ALIAS
// payload — callers that retain them must copy.
func ParseDataRowInto(dst [][]byte, payload []byte) ([][]byte, error) {
	if len(payload) < 2 {
		return nil, errors.New("postgres/protocol: short DataRow")
	}
	pos := 0
	n, err := ReadInt16(payload, &pos)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("postgres/protocol: negative field count %d", n)
	}
	var fields [][]byte
	if cap(dst) >= int(n) {
		fields = dst[:n]
	} else {
		fields = make([][]byte, n)
	}
	for i := range fields {
		if pos+4 > len(payload) {
			return nil, errors.New("postgres/protocol: short DataRow field length")
		}
		flen := int32(binary.BigEndian.Uint32(payload[pos:]))
		pos += 4
		if flen < 0 {
			fields[i] = nil
			continue
		}
		if pos+int(flen) > len(payload) {
			return nil, errors.New("postgres/protocol: short DataRow field body")
		}
		fields[i] = payload[pos : pos+int(flen)]
		pos += int(flen)
	}
	return fields, nil
}

// ParseCommandComplete returns the tag string (e.g., "SELECT 2", "INSERT 0 1").
func ParseCommandComplete(payload []byte) (string, error) {
	pos := 0
	return ReadCString(payload, &pos)
}

// commandCompleteTagBytes returns a byte-slice view of the CommandComplete
// tag (excluding the trailing null). The returned slice ALIASES payload —
// callers that retain it beyond the next Reader operation must copy.
func commandCompleteTagBytes(payload []byte) []byte {
	for i := 0; i < len(payload); i++ {
		if payload[i] == 0 {
			return payload[:i]
		}
	}
	return payload
}

// RowsAffectedBytes is the []byte-input equivalent of RowsAffected. It
// parses the row count out of a CommandComplete tag without requiring the
// caller to first allocate a string. Returns (0, false) if the tag does
// not carry a row count.
func RowsAffectedBytes(tag []byte) (int64, bool) {
	// Split on spaces without allocating. Walk back from the end to find
	// the last digit-only token — that's the row count for every variant
	// PG emits (see RowsAffected for the grammar).
	n := len(tag)
	if n == 0 {
		return 0, false
	}
	// Trim trailing spaces.
	for n > 0 && tag[n-1] == ' ' {
		n--
	}
	if n == 0 {
		return 0, false
	}
	end := n
	start := n - 1
	for start > 0 && tag[start-1] != ' ' {
		start--
	}
	last := tag[start:end]
	// Require first token to be one of the known verbs; otherwise not a
	// row-count tag.
	var verbEnd int
	for verbEnd = 0; verbEnd < len(tag); verbEnd++ {
		if tag[verbEnd] == ' ' {
			break
		}
	}
	verb := tag[:verbEnd]
	if !isRowCountVerb(verb) {
		return 0, false
	}
	// INSERT has "INSERT oid rows" — last word is still rows.
	var v int64
	for _, b := range last {
		if b < '0' || b > '9' {
			return 0, false
		}
		v = v*10 + int64(b-'0')
	}
	return v, true
}

func isRowCountVerb(verb []byte) bool {
	switch string(verb) { // string(verb) in switch does NOT allocate — compiler specializes
	case "INSERT", "SELECT", "UPDATE", "DELETE", "MOVE", "FETCH", "COPY":
		return true
	}
	return false
}

// RowsAffected extracts the row count from a CommandComplete tag. Returns
// (0, false) if the tag does not carry a row count.
//
// Tags and where their count lives (0-indexed):
//
//	INSERT oid rows   -> rows is word 2
//	DELETE rows       -> rows is word 1
//	UPDATE rows       -> rows is word 1
//	SELECT rows       -> rows is word 1
//	MOVE rows         -> rows is word 1
//	FETCH rows        -> rows is word 1
//	COPY rows         -> rows is word 1
func RowsAffected(tag string) (int64, bool) {
	parts := strings.Fields(tag)
	if len(parts) == 0 {
		return 0, false
	}
	var idx int
	switch parts[0] {
	case "INSERT":
		if len(parts) < 3 {
			return 0, false
		}
		idx = 2
	case "SELECT", "UPDATE", "DELETE", "MOVE", "FETCH", "COPY":
		if len(parts) < 2 {
			return 0, false
		}
		idx = 1
	default:
		return 0, false
	}
	n, err := strconv.ParseInt(parts[idx], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// simpleQueryPhase tracks the state of a SimpleQueryState machine.
type simpleQueryPhase int

const (
	sqPhaseAwaitResult     simpleQueryPhase = iota // expect T, I, C, E
	sqPhaseAwaitRowsOrDone                         // after RowDescription: expect D, C
	sqPhaseDone                                    // ReadyForQuery seen
)

// SimpleQueryState drives the client side of a Query ('Q') round trip.
// After calling WriteQuery, feed server messages one at a time via Handle.
// Handle returns done=true after ReadyForQuery.
//
// Multi-statement queries are supported: after CommandComplete the state
// machine returns to phaseAwaitResult, accepting another RowDescription.
type SimpleQueryState struct {
	Columns []ColumnDesc
	// Tag is left empty on CommandComplete to avoid the per-query string
	// allocation. Callers should use TagBytes() and RowsAffectedBytes()
	// instead. If a caller needs the tag as a Go string, they can
	// materialize it on demand via string(q.TagBytes()).
	//
	// Deprecated: use TagBytes() for allocation-free access.
	Tag string
	Err *PGError

	phase simpleQueryPhase
	// fieldScratch is reused across DataRow payloads within a single
	// result set. Handle hands it to ParseDataRowInto so a 1-col row only
	// allocates once per pgRequest life cycle (and zero times if the
	// backing array from the previous cycle is still large enough).
	fieldScratch [][]byte
	// tagBuf holds a copy of the CommandComplete tag bytes. Reused across
	// life cycles to avoid the per-Query string allocation that the
	// previous implementation paid via ReadCString.
	tagBuf []byte
	// tagDirty indicates tagBuf has fresh bytes that Tag has not yet been
	// materialized from. Currently unused; retained for future use if we
	// re-introduce lazy string materialization for back-compat.
	tagDirty bool
}

// TagBytes returns the CommandComplete tag as an owned byte slice
// (independent of the wire Reader's buffer). Callers that only need to
// parse a row count (via RowsAffectedBytes) should use this instead of
// Tag to avoid the string allocation.
func (q *SimpleQueryState) TagBytes() []byte { return q.tagBuf }

// Reset zeroes the state machine for reuse while preserving the internal
// fieldScratch / Columns / tagBuf backing arrays. The caller still owns the
// semantic fields (Columns, Tag, Err): they are re-set to their zero
// values with length=0 but cap retained.
func (q *SimpleQueryState) Reset() {
	scratch := q.fieldScratch
	cols := q.Columns
	tagBuf := q.tagBuf
	*q = SimpleQueryState{}
	if scratch != nil {
		q.fieldScratch = scratch[:0]
	}
	if cols != nil {
		q.Columns = cols[:0]
	}
	if tagBuf != nil {
		q.tagBuf = tagBuf[:0]
	}
}

// Handle processes one server message. onRowDesc is invoked once per
// RowDescription; onRow once per DataRow (payload slices alias Reader
// memory — copy if retention is needed). Either callback may be nil.
func (q *SimpleQueryState) Handle(
	msgType byte, payload []byte,
	onRowDesc func([]ColumnDesc),
	onRow func([][]byte),
) (bool, error) {
	switch msgType {
	case BackendRowDescription:
		cols, err := ParseRowDescriptionInto(q.Columns, payload)
		if err != nil {
			return false, err
		}
		q.Columns = cols
		if onRowDesc != nil {
			onRowDesc(cols)
		}
		q.phase = sqPhaseAwaitRowsOrDone
		return false, nil
	case BackendDataRow:
		if q.phase != sqPhaseAwaitRowsOrDone {
			return false, errors.New("postgres/protocol: DataRow outside of result set")
		}
		fields, err := ParseDataRowInto(q.fieldScratch, payload)
		if err != nil {
			return false, err
		}
		q.fieldScratch = fields
		if onRow != nil {
			onRow(fields)
		}
		return false, nil
	case BackendCommandComplete:
		tagBytes := commandCompleteTagBytes(payload)
		// Store the tag into an owned byte buffer; materialize q.Tag
		// (string) via the Tag accessor only when accessed. This keeps
		// Query/Exec hot paths allocation-free when the caller uses
		// TagBytes + RowsAffectedBytes, while still giving Tag-reading
		// callers (tests, multi-statement enumerations, custom
		// integrations) the string form on demand.
		q.tagBuf = append(q.tagBuf[:0], tagBytes...)
		q.Tag = ""
		q.tagDirty = true
		// Multi-statement: another RowDescription may follow before RFQ.
		q.phase = sqPhaseAwaitResult
		return false, nil
	case BackendEmptyQuery:
		q.Tag = ""
		q.phase = sqPhaseAwaitResult
		return false, nil
	case BackendErrorResponse:
		q.Err = ParseErrorResponse(payload)
		// Server will still send RFQ; stay in sqPhaseAwaitResult.
		q.phase = sqPhaseAwaitResult
		return false, nil
	case BackendNoticeResponse, BackendParameterStatus, BackendNotification:
		// Out-of-band messages are harmless; ignore.
		return false, nil
	case BackendReadyForQuery:
		q.phase = sqPhaseDone
		if q.Err != nil {
			return true, q.Err
		}
		return true, nil
	default:
		return false, fmt.Errorf("postgres/protocol: unexpected message %q in simple query", msgType)
	}
}

// WriteQuery encodes a Query ('Q') message into w and returns a copy of the
// resulting bytes.
func WriteQuery(w *Writer, sql string) []byte {
	w.Reset()
	w.StartMessage(MsgQuery)
	w.WriteString(sql)
	w.FinishMessage()
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// WriteQueryInto encodes a Query ('Q') message into w's buffer without
// making a separate copy. The returned slice aliases w.Bytes(); the caller
// must consume it (by passing to loop.Write, which copies before returning)
// before any other goroutine can Reset the writer.
//
// This is the zero-alloc variant of WriteQuery. Use it when the caller can
// hold the writer's lock across the loop.Write call.
func WriteQueryInto(w *Writer, sql string) []byte {
	w.Reset()
	w.StartMessage(MsgQuery)
	w.WriteString(sql)
	w.FinishMessage()
	return w.Bytes()
}
