package protocol

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func buildCopyResponse(format int8, columnFormats []int16) []byte {
	b := make([]byte, 3+2*len(columnFormats))
	b[0] = byte(format)
	binary.BigEndian.PutUint16(b[1:3], uint16(len(columnFormats)))
	for i, f := range columnFormats {
		binary.BigEndian.PutUint16(b[3+i*2:], uint16(f))
	}
	return b
}

func TestCopyBinaryHeaderShape(t *testing.T) {
	if len(CopyBinaryHeader) != 19 {
		t.Fatalf("header len=%d want 19", len(CopyBinaryHeader))
	}
	if string(CopyBinaryHeader[:11]) != "PGCOPY\n\xff\r\n\x00" {
		t.Fatalf("signature mismatch: %q", CopyBinaryHeader[:11])
	}
	// flags field = 0
	if binary.BigEndian.Uint32(CopyBinaryHeader[11:15]) != 0 {
		t.Fatal("flags must be 0")
	}
	// header ext length = 0
	if binary.BigEndian.Uint32(CopyBinaryHeader[15:19]) != 0 {
		t.Fatal("header ext length must be 0")
	}
	if !bytes.Equal(CopyBinaryTrailer, []byte{0xff, 0xff}) {
		t.Fatalf("trailer=%v", CopyBinaryTrailer)
	}
}

func TestParseCopyResponse(t *testing.T) {
	resp, err := ParseCopyResponse(buildCopyResponse(1, []int16{1, 0, 1}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Format != 1 || resp.NumColumns != 3 {
		t.Fatalf("resp=%+v", resp)
	}
	if len(resp.ColumnFormats) != 3 || resp.ColumnFormats[1] != 0 {
		t.Fatalf("cols=%v", resp.ColumnFormats)
	}
}

func TestWriteCopyMessages(t *testing.T) {
	w := NewWriter()
	d := WriteCopyData(w, []byte("row-bytes"))
	if d[0] != MsgCopyData {
		t.Fatalf("copy data type=%q", d[0])
	}
	done := WriteCopyDone(w)
	if done[0] != MsgCopyDone || len(done) != 5 {
		t.Fatalf("copy done: %v", done)
	}
	fail := WriteCopyFail(w, "oops")
	if fail[0] != MsgCopyFail {
		t.Fatalf("copy fail type=%q", fail[0])
	}
	// payload should be "oops\x00"
	if string(fail[5:]) != "oops\x00" {
		t.Fatalf("copy fail payload=%q", fail[5:])
	}
}

func TestCopyInStateHappyPath(t *testing.T) {
	s := &CopyInState{}
	if _, err := s.Handle(BackendCopyInResponse, buildCopyResponse(0, []int16{0, 0})); err != nil {
		t.Fatal(err)
	}
	if !s.Ready() {
		t.Fatal("expected Ready() after CopyInResponse")
	}
	// Driver streams CopyData (no state change) then sends CopyDone; server
	// responds with CommandComplete + RFQ.
	if _, err := s.Handle(BackendCommandComplete, buildCommandComplete("COPY 3")); err != nil {
		t.Fatal(err)
	}
	if s.Tag != "COPY 3" {
		t.Fatalf("tag=%q", s.Tag)
	}
	done, err := s.Handle(BackendReadyForQuery, []byte{'I'})
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

func TestCopyInStateError(t *testing.T) {
	s := &CopyInState{}
	s.Handle(BackendCopyInResponse, buildCopyResponse(0, nil))
	// Server rejects input (e.g. constraint violation).
	payload := []byte("SERROR\x00C23505\x00Mduplicate\x00\x00")
	s.Handle(BackendErrorResponse, payload)
	done, err := s.Handle(BackendReadyForQuery, []byte{'I'})
	if !done || err == nil {
		t.Fatalf("expected done with surfaced error: done=%v err=%v", done, err)
	}
}

func TestCopyOutStateHappyPath(t *testing.T) {
	s := &CopyOutState{}
	var rows [][]byte
	onRow := func(b []byte) { rows = append(rows, append([]byte(nil), b...)) }

	if _, err := s.Handle(BackendCopyOutResponse, buildCopyResponse(0, []int16{0}), onRow); err != nil {
		t.Fatal(err)
	}
	s.Handle(BackendCopyData, []byte("row-1\n"), onRow)
	s.Handle(BackendCopyData, []byte("row-2\n"), onRow)
	if _, err := s.Handle(BackendCopyDone, nil, onRow); err != nil {
		t.Fatal(err)
	}
	s.Handle(BackendCommandComplete, buildCommandComplete("COPY 2"), onRow)
	done, err := s.Handle(BackendReadyForQuery, []byte{'I'}, onRow)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(rows) != 2 || string(rows[0]) != "row-1\n" {
		t.Fatalf("rows=%v", rows)
	}
	if s.Tag != "COPY 2" {
		t.Fatalf("tag=%q", s.Tag)
	}
}

func TestCopyOutStateRejectCopyDataOutsideStreaming(t *testing.T) {
	s := &CopyOutState{}
	// CopyData without preceding CopyOutResponse should error.
	if _, err := s.Handle(BackendCopyData, []byte("x"), nil); err == nil {
		t.Fatal("expected error on out-of-phase CopyData")
	}
}
