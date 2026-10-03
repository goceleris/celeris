package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PreparedStmt is the post-describe metadata for a prepared statement.
type PreparedStmt struct {
	Name      string // "" for unnamed statement
	Query     string
	ParamOIDs []uint32
	Columns   []ColumnDesc
}

// WriteParse encodes a Parse ('P') message and returns a copy of the bytes.
//
// Format:
//
//	'P' int32 len | string name | string query | int16 nparams | int32 oids[nparams]
func WriteParse(w *Writer, name, query string, paramOIDs []uint32) []byte {
	w.Reset()
	w.StartMessage(MsgParse)
	w.WriteString(name)
	w.WriteString(query)
	w.WriteInt16(int16(len(paramOIDs)))
	for _, oid := range paramOIDs {
		w.WriteInt32(int32(oid))
	}
	w.FinishMessage()
	return snapshot(w)
}

// WriteBind encodes a Bind ('B') message and returns a copy of the bytes.
//
// paramFormats may be:
//   - empty      — all params are text (format 0)
//   - length 1   — format code applies to every param
//   - length N   — one code per param
//
// resultFormats follows the same convention for result columns.
//
// paramValues are the already-encoded bytes; nil means SQL NULL (length -1).
func WriteBind(w *Writer, portal, stmt string, paramFormats []int16, paramValues [][]byte, resultFormats []int16) []byte {
	w.Reset()
	w.StartMessage(MsgBind)
	w.WriteString(portal)
	w.WriteString(stmt)
	w.WriteInt16(int16(len(paramFormats)))
	for _, f := range paramFormats {
		w.WriteInt16(f)
	}
	w.WriteInt16(int16(len(paramValues)))
	for _, v := range paramValues {
		if v == nil {
			w.WriteInt32(-1)
			continue
		}
		w.WriteInt32(int32(len(v)))
		w.WriteBytes(v)
	}
	w.WriteInt16(int16(len(resultFormats)))
	for _, f := range resultFormats {
		w.WriteInt16(f)
	}
	w.FinishMessage()
	return snapshot(w)
}

// WriteDescribe encodes a Describe ('D') message. kind is 'S' (statement)
// or 'P' (portal).
func WriteDescribe(w *Writer, kind byte, name string) []byte {
	if kind != 'S' && kind != 'P' {
		panic("postgres/protocol: Describe kind must be 'S' or 'P'")
	}
	w.Reset()
	w.StartMessage(MsgDescribe)
	_ = w.WriteByte(kind)
	w.WriteString(name)
	w.FinishMessage()
	return snapshot(w)
}

// WriteExecute encodes an Execute ('E') message. maxRows=0 means all rows.
func WriteExecute(w *Writer, portal string, maxRows int32) []byte {
	w.Reset()
	w.StartMessage(MsgExecute)
	w.WriteString(portal)
	w.WriteInt32(maxRows)
	w.FinishMessage()
	return snapshot(w)
}

// WriteSync encodes a Sync ('S') message.
func WriteSync(w *Writer) []byte {
	w.Reset()
	w.StartMessage(MsgSync)
	w.FinishMessage()
	return snapshot(w)
}

// WriteClose encodes a Close ('C') message. kind is 'S' or 'P'.
func WriteClose(w *Writer, kind byte, name string) []byte {
	if kind != 'S' && kind != 'P' {
		panic("postgres/protocol: Close kind must be 'S' or 'P'")
	}
	w.Reset()
	w.StartMessage(MsgClose)
	_ = w.WriteByte(kind)
	w.WriteString(name)
	w.FinishMessage()
	return snapshot(w)
}

// WriteFlush encodes a Flush ('H') message.
func WriteFlush(w *Writer) []byte {
	w.Reset()
	w.StartMessage(MsgFlush)
	w.FinishMessage()
	return snapshot(w)
}

// snapshot returns a copy of the Writer's current buffer so callers can
// reuse the Writer without aliasing.
func snapshot(w *Writer) []byte {
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// ---------- Append variants (no reset, no snapshot) ----------
//
// These write directly into the Writer's existing buffer and return nothing.
// The caller calls w.Reset() once at the start, appends multiple messages via
// the Append* functions, then takes a single snapshot (or passes w.Bytes()
// directly to Write which copies). This eliminates N allocations + joinBytes
// for an N-message pipeline.

// AppendBind appends a Bind ('B') message to the Writer without resetting.
func AppendBind(w *Writer, portal, stmt string, paramFormats []int16, paramValues [][]byte, resultFormats []int16) {
	w.StartMessage(MsgBind)
	w.WriteString(portal)
	w.WriteString(stmt)
	w.WriteInt16(int16(len(paramFormats)))
	for _, f := range paramFormats {
		w.WriteInt16(f)
	}
	w.WriteInt16(int16(len(paramValues)))
	for _, v := range paramValues {
		if v == nil {
			w.WriteInt32(-1)
			continue
		}
		w.WriteInt32(int32(len(v)))
		w.WriteBytes(v)
	}
	w.WriteInt16(int16(len(resultFormats)))
	for _, f := range resultFormats {
		w.WriteInt16(f)
	}
	w.FinishMessage()
}

// AppendParse appends a Parse ('P') message to the Writer without resetting.
func AppendParse(w *Writer, name, query string, paramOIDs []uint32) {
	w.StartMessage(MsgParse)
	w.WriteString(name)
	w.WriteString(query)
	w.WriteInt16(int16(len(paramOIDs)))
	for _, oid := range paramOIDs {
		w.WriteInt32(int32(oid))
	}
	w.FinishMessage()
}

// AppendDescribe appends a Describe ('D') message to the Writer without resetting.
func AppendDescribe(w *Writer, kind byte, name string) {
	w.StartMessage(MsgDescribe)
	_ = w.WriteByte(kind)
	w.WriteString(name)
	w.FinishMessage()
}

// AppendExecute appends an Execute ('E') message to the Writer without resetting.
func AppendExecute(w *Writer, portal string, maxRows int32) {
	w.StartMessage(MsgExecute)
	w.WriteString(portal)
	w.WriteInt32(maxRows)
	w.FinishMessage()
}

// AppendSync appends a Sync ('S') message to the Writer without resetting.
func AppendSync(w *Writer) {
	w.StartMessage(MsgSync)
	w.FinishMessage()
}

// ParseParameterDescription returns the parameter type OIDs from a 't'
// message payload.
func ParseParameterDescription(payload []byte) ([]uint32, error) {
	if len(payload) < 2 {
		return nil, errors.New("postgres/protocol: short ParameterDescription")
	}
	n := int16(binary.BigEndian.Uint16(payload[:2]))
	if n < 0 {
		return nil, fmt.Errorf("postgres/protocol: negative param count %d", n)
	}
	if len(payload) < 2+int(n)*4 {
		return nil, errors.New("postgres/protocol: short ParameterDescription OIDs")
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(payload[2+i*4:])
	}
	return out, nil
}

// extendedPhase tracks state of an ExtendedQueryState machine.
type extendedPhase int

const (
	exPhaseExpectParseComplete extendedPhase = iota
	exPhaseExpectBindComplete
	exPhaseExpectDescribeResult
	exPhaseExpectExecuteResult
	exPhaseExpectReady
	exPhaseDone
	exPhaseErrored // error seen; wait for RFQ
)

// ExtendedQueryState drives a Parse → Bind → Describe → Execute → Sync
// round trip. Any ErrorResponse from the server triggers the error phase;
// the state machine then drains messages until ReadyForQuery, at which
// point Handle returns done=true with the surfaced PGError.
type ExtendedQueryState struct {
	// Set true when the client sent a Describe S or Describe P. If false
	// the server will not emit ParameterDescription / RowDescription and
	// the state machine jumps straight from BindComplete to executing.
	HasDescribe bool

	// SkipParse is true when the client re-uses an already-parsed statement
	// on this conn and did not emit a Parse message — so no ParseComplete is
	// expected. The state machine starts at ExpectBindComplete.
	SkipParse bool

	ParamOIDs []uint32     // from ParameterDescription (if Describe S)
	Columns   []ColumnDesc // from RowDescription / NoData
	Tag       string       // materialized on access via TagBytes (see below)
	Err       *PGError

	phase extendedPhase
	init  bool
	// fieldScratch is reused across DataRow payloads within a single
	// execute. Handle hands it to ParseDataRowInto so small result sets
	// avoid a per-row [][]byte allocation after the first row.
	fieldScratch [][]byte
	// tagBuf holds the CommandComplete tag bytes in an owned buffer. Reused
	// across life cycles; TagBytes returns a view of it without
	// materializing a string.
	tagBuf []byte
}

// TagBytes returns the CommandComplete tag as an owned byte slice.
// Callers that only need RowsAffected should prefer
// protocol.RowsAffectedBytes(e.TagBytes()) over Tag to skip the string
// allocation.
func (e *ExtendedQueryState) TagBytes() []byte { return e.tagBuf }

// Reset zeroes the state machine for reuse while preserving the internal
// fieldScratch / Columns / ParamOIDs / tagBuf backing arrays.
func (e *ExtendedQueryState) Reset() {
	scratch := e.fieldScratch
	cols := e.Columns
	oids := e.ParamOIDs
	tagBuf := e.tagBuf
	*e = ExtendedQueryState{}
	if scratch != nil {
		e.fieldScratch = scratch[:0]
	}
	if cols != nil {
		e.Columns = cols[:0]
	}
	if oids != nil {
		e.ParamOIDs = oids[:0]
	}
	if tagBuf != nil {
		e.tagBuf = tagBuf[:0]
	}
}

// ExtendedQueryObserver is the dispatch sink for [ExtendedQueryState.Handle].
// Same rationale as [SimpleQueryObserver]: passing an interface lets the
// caller share an allocation-free dispatch object across many Handle
// invocations. nil disables per-row dispatch.
type ExtendedQueryObserver interface {
	OnRow(fields [][]byte)
}

// ExtendedHandleCallback adapts a function value into an
// [ExtendedQueryObserver] for callers (e.g. tests) that prefer closure
// semantics. Allocates a function pointer per construction.
type ExtendedHandleCallback func([][]byte)

// OnRow implements ExtendedQueryObserver.
func (f ExtendedHandleCallback) OnRow(fields [][]byte) { f(fields) }

// Handle consumes one server message. obs may be nil to disable per-row
// dispatch. Payload alias rules match SimpleQueryState.
func (e *ExtendedQueryState) Handle(
	msgType byte, payload []byte,
	obs ExtendedQueryObserver,
) (bool, error) {
	if !e.init {
		if e.SkipParse {
			e.phase = exPhaseExpectBindComplete
		}
		e.init = true
	}
	switch msgType {
	case BackendErrorResponse:
		e.Err = ParseErrorResponse(payload)
		e.phase = exPhaseErrored
		return false, nil
	case BackendNoticeResponse, BackendParameterStatus, BackendNotification:
		return false, nil
	case BackendReadyForQuery:
		e.phase = exPhaseDone
		if e.Err != nil {
			return true, e.Err
		}
		return true, nil
	}

	if e.phase == exPhaseErrored {
		// Discard everything until RFQ.
		return false, nil
	}

	switch msgType {
	case BackendParseComplete:
		if e.phase != exPhaseExpectParseComplete {
			return false, unexpected("ParseComplete", e.phase)
		}
		e.phase = exPhaseExpectBindComplete
		return false, nil
	case BackendBindComplete:
		if e.phase != exPhaseExpectBindComplete {
			return false, unexpected("BindComplete", e.phase)
		}
		if e.HasDescribe {
			e.phase = exPhaseExpectDescribeResult
		} else {
			e.phase = exPhaseExpectExecuteResult
		}
		return false, nil
	case BackendParameterDesc:
		if e.phase != exPhaseExpectDescribeResult {
			return false, unexpected("ParameterDescription", e.phase)
		}
		oids, err := ParseParameterDescription(payload)
		if err != nil {
			return false, err
		}
		e.ParamOIDs = oids
		// Still expecting RowDescription or NoData.
		return false, nil
	case BackendRowDescription:
		if e.phase != exPhaseExpectDescribeResult {
			return false, unexpected("RowDescription", e.phase)
		}
		cols, err := ParseRowDescriptionInto(e.Columns, payload)
		if err != nil {
			return false, err
		}
		e.Columns = cols
		e.phase = exPhaseExpectExecuteResult
		return false, nil
	case BackendNoData:
		if e.phase != exPhaseExpectDescribeResult {
			return false, unexpected("NoData", e.phase)
		}
		e.Columns = nil
		e.phase = exPhaseExpectExecuteResult
		return false, nil
	case BackendDataRow:
		if e.phase != exPhaseExpectExecuteResult {
			return false, unexpected("DataRow", e.phase)
		}
		fields, err := ParseDataRowInto(e.fieldScratch, payload)
		if err != nil {
			return false, err
		}
		e.fieldScratch = fields
		if obs != nil {
			obs.OnRow(fields)
		}
		return false, nil
	case BackendCommandComplete:
		if e.phase != exPhaseExpectExecuteResult {
			return false, unexpected("CommandComplete", e.phase)
		}
		tagBytes := commandCompleteTagBytes(payload)
		e.tagBuf = append(e.tagBuf[:0], tagBytes...)
		// Clear the string form; callers that need it must materialize
		// from TagBytes. Keeps the hot Query path allocation-free.
		e.Tag = ""
		e.phase = exPhaseExpectReady
		return false, nil
	case BackendEmptyQuery:
		if e.phase != exPhaseExpectExecuteResult {
			return false, unexpected("EmptyQuery", e.phase)
		}
		e.phase = exPhaseExpectReady
		return false, nil
	case BackendPortalSuspended:
		// Execute hit its row limit; portal is suspended and ReadyForQuery
		// will follow.
		e.phase = exPhaseExpectReady
		return false, nil
	case BackendCloseComplete:
		// A Close S/P sent inline with Sync produces this — accept and
		// continue without a state transition.
		return false, nil
	default:
		return false, fmt.Errorf("postgres/protocol: unexpected message %q in extended query", msgType)
	}
}

func unexpected(got string, phase extendedPhase) error {
	return fmt.Errorf("postgres/protocol: unexpected %s in extended query (phase=%d)", got, phase)
}
