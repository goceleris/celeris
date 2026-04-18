package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// CopyBinaryHeader is the 19-byte fixed header required at the start of a
// binary-format COPY IN / COPY OUT stream. Layout:
//
//	"PGCOPY\n\377\r\n\0" (11 bytes) | int32 flags (=0) | int32 header ext length (=0)
var CopyBinaryHeader = []byte{
	'P', 'G', 'C', 'O', 'P', 'Y', '\n', 0xff, '\r', '\n', 0,
	0, 0, 0, 0, // flags field
	0, 0, 0, 0, // header extension area length
}

// CopyBinaryTrailer is the 2-byte trailer (int16 = -1) marking the end of a
// binary COPY stream.
var CopyBinaryTrailer = []byte{0xff, 0xff}

// CopyResponse holds CopyInResponse / CopyOutResponse / CopyBothResponse
// metadata.
type CopyResponse struct {
	Format        int8 // 0=text, 1=binary
	NumColumns    int16
	ColumnFormats []int16 // length == NumColumns
}

// ParseCopyResponse parses a Copy{In,Out,Both}Response payload.
//
// Format:
//
//	int8 overall format | int16 N | int16 per-column format * N
func ParseCopyResponse(payload []byte) (CopyResponse, error) {
	var out CopyResponse
	if len(payload) < 3 {
		return out, errors.New("postgres/protocol: short CopyResponse")
	}
	out.Format = int8(payload[0])
	out.NumColumns = int16(binary.BigEndian.Uint16(payload[1:3]))
	if out.NumColumns < 0 {
		return out, fmt.Errorf("postgres/protocol: negative column count %d", out.NumColumns)
	}
	if len(payload) < 3+int(out.NumColumns)*2 {
		return out, errors.New("postgres/protocol: short CopyResponse column list")
	}
	out.ColumnFormats = make([]int16, out.NumColumns)
	for i := range out.ColumnFormats {
		out.ColumnFormats[i] = int16(binary.BigEndian.Uint16(payload[3+i*2:]))
	}
	return out, nil
}

// WriteCopyData encodes a CopyData ('d') message carrying rowBytes.
func WriteCopyData(w *Writer, rowBytes []byte) []byte {
	w.Reset()
	w.StartMessage(MsgCopyData)
	w.WriteBytes(rowBytes)
	w.FinishMessage()
	return snapshot(w)
}

// WriteCopyDone encodes a CopyDone ('c') message.
func WriteCopyDone(w *Writer) []byte {
	w.Reset()
	w.StartMessage(MsgCopyDone)
	w.FinishMessage()
	return snapshot(w)
}

// WriteCopyFail encodes a CopyFail ('f') message with the given reason.
func WriteCopyFail(w *Writer, reason string) []byte {
	w.Reset()
	w.StartMessage(MsgCopyFail)
	w.WriteString(reason)
	w.FinishMessage()
	return snapshot(w)
}

type copyInPhase int

const (
	copyInWaiting  copyInPhase = iota // awaiting CopyInResponse
	copyInReady                       // client should stream CopyData
	copyInFinished                    // client sent CopyDone/CopyFail
	copyInDone                        // CommandComplete seen
)

// CopyInState tracks the client side of a COPY FROM STDIN exchange. After
// sending a Query that triggers COPY FROM, the driver feeds incoming
// messages here. Once Resp is populated and phase is ready, the driver
// streams CopyData messages and concludes with CopyDone or CopyFail.
type CopyInState struct {
	Resp CopyResponse
	Tag  string
	Err  *PGError

	phase copyInPhase
}

// Ready reports whether the server has signaled CopyInResponse — i.e. the
// driver may now send CopyData.
func (s *CopyInState) Ready() bool {
	return s.phase == copyInReady || s.phase == copyInFinished
}

// Handle processes one incoming server message.
func (s *CopyInState) Handle(msgType byte, payload []byte) (bool, error) {
	switch msgType {
	case BackendCopyInResponse:
		if s.phase != copyInWaiting {
			return false, errors.New("postgres/protocol: unexpected CopyInResponse")
		}
		resp, err := ParseCopyResponse(payload)
		if err != nil {
			return false, err
		}
		s.Resp = resp
		s.phase = copyInReady
		return false, nil
	case BackendCommandComplete:
		tag, err := ParseCommandComplete(payload)
		if err != nil {
			return false, err
		}
		s.Tag = tag
		s.phase = copyInDone
		return false, nil
	case BackendErrorResponse:
		s.Err = ParseErrorResponse(payload)
		s.phase = copyInDone
		return false, nil
	case BackendNoticeResponse, BackendParameterStatus, BackendNotification:
		return false, nil
	case BackendReadyForQuery:
		if s.Err != nil {
			return true, s.Err
		}
		return true, nil
	default:
		return false, fmt.Errorf("postgres/protocol: unexpected message %q in COPY IN", msgType)
	}
}

type copyOutPhase int

const (
	copyOutWaiting copyOutPhase = iota
	copyOutStreaming
	copyOutFinished
)

// CopyOutState tracks the client side of a COPY TO STDOUT exchange.
type CopyOutState struct {
	Resp CopyResponse
	Tag  string
	Err  *PGError

	phase copyOutPhase
}

// Handle processes one incoming message. onRow is called for each CopyData
// message; the slice alias rules match the other state machines (callers
// must copy for retention).
func (s *CopyOutState) Handle(msgType byte, payload []byte, onRow func([]byte)) (bool, error) {
	switch msgType {
	case BackendCopyOutResponse:
		if s.phase != copyOutWaiting {
			return false, errors.New("postgres/protocol: unexpected CopyOutResponse")
		}
		resp, err := ParseCopyResponse(payload)
		if err != nil {
			return false, err
		}
		s.Resp = resp
		s.phase = copyOutStreaming
		return false, nil
	case BackendCopyData:
		if s.phase != copyOutStreaming {
			return false, errors.New("postgres/protocol: CopyData outside streaming phase")
		}
		if onRow != nil {
			onRow(payload)
		}
		return false, nil
	case BackendCopyDone:
		if s.phase != copyOutStreaming {
			return false, errors.New("postgres/protocol: CopyDone outside streaming phase")
		}
		s.phase = copyOutFinished
		return false, nil
	case BackendCommandComplete:
		tag, err := ParseCommandComplete(payload)
		if err != nil {
			return false, err
		}
		s.Tag = tag
		return false, nil
	case BackendErrorResponse:
		s.Err = ParseErrorResponse(payload)
		return false, nil
	case BackendNoticeResponse, BackendParameterStatus, BackendNotification:
		return false, nil
	case BackendReadyForQuery:
		if s.Err != nil {
			return true, s.Err
		}
		return true, nil
	default:
		return false, fmt.Errorf("postgres/protocol: unexpected message %q in COPY OUT", msgType)
	}
}
