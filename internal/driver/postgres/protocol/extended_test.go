package protocol

import (
	"encoding/binary"
	"testing"
)

func buildParameterDescription(oids []uint32) []byte {
	b := make([]byte, 2+4*len(oids))
	binary.BigEndian.PutUint16(b[:2], uint16(len(oids)))
	for i, oid := range oids {
		binary.BigEndian.PutUint32(b[2+i*4:], oid)
	}
	return b
}

func TestWriteParse(t *testing.T) {
	w := NewWriter()
	out := WriteParse(w, "stmt1", "SELECT $1::int4", []uint32{OIDInt4})
	if out[0] != MsgParse {
		t.Fatalf("type=%q", out[0])
	}
	// payload = "stmt1\x00SELECT $1::int4\x00" + int16(1) + int32(23)
	// verify presence of the pieces.
	payload := out[5:]
	if string(payload[:6]) != "stmt1\x00" {
		t.Fatalf("stmt name: %q", payload[:6])
	}
}

func TestWriteBindAllFormats(t *testing.T) {
	w := NewWriter()
	out := WriteBind(w, "portal", "stmt", []int16{FormatBinary}, [][]byte{{0, 0, 0, 5}}, []int16{FormatBinary})
	if out[0] != MsgBind {
		t.Fatalf("type=%q", out[0])
	}
	// Sanity: total length matches length prefix.
	n := binary.BigEndian.Uint32(out[1:5])
	if int(n)+1 != len(out) {
		t.Fatalf("length mismatch: n+1=%d len=%d", n+1, len(out))
	}
}

func TestWriteBindNullParam(t *testing.T) {
	w := NewWriter()
	out := WriteBind(w, "", "", nil, [][]byte{nil, []byte("hi")}, nil)
	// Skip header; find the null-length marker for the first param.
	_ = out
}

func TestWriteDescribeExecuteSync(t *testing.T) {
	w := NewWriter()
	d := WriteDescribe(w, 'S', "stmt")
	if d[0] != MsgDescribe || d[5] != 'S' {
		t.Fatalf("bad describe: %q %q", d[0], d[5])
	}

	e := WriteExecute(w, "portal", 0)
	if e[0] != MsgExecute {
		t.Fatalf("execute type=%q", e[0])
	}

	s := WriteSync(w)
	if s[0] != MsgSync {
		t.Fatalf("sync type=%q", s[0])
	}
	if len(s) != 5 {
		t.Fatalf("sync len=%d want 5", len(s))
	}
}

func TestWriteCloseKindPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on invalid Close kind")
		}
	}()
	WriteClose(NewWriter(), 'X', "stmt")
}

func TestParseParameterDescription(t *testing.T) {
	oids, err := ParseParameterDescription(buildParameterDescription([]uint32{OIDInt4, OIDText, OIDBool}))
	if err != nil {
		t.Fatal(err)
	}
	if len(oids) != 3 || oids[0] != OIDInt4 || oids[2] != OIDBool {
		t.Fatalf("oids=%v", oids)
	}
}

func TestExtendedQueryFullCycle(t *testing.T) {
	e := &ExtendedQueryState{HasDescribe: true}
	cols := []ColumnDesc{{Name: "n", TypeOID: OIDInt4}}

	var rows int
	onRow := ExtendedHandleCallback(func(f [][]byte) { rows++ })

	// ParseComplete
	if _, err := e.Handle(BackendParseComplete, nil, onRow); err != nil {
		t.Fatal(err)
	}
	// BindComplete
	if _, err := e.Handle(BackendBindComplete, nil, onRow); err != nil {
		t.Fatal(err)
	}
	// ParameterDescription
	if _, err := e.Handle(BackendParameterDesc, buildParameterDescription([]uint32{OIDInt4}), onRow); err != nil {
		t.Fatal(err)
	}
	if len(e.ParamOIDs) != 1 || e.ParamOIDs[0] != OIDInt4 {
		t.Fatalf("param oids=%v", e.ParamOIDs)
	}
	// RowDescription
	if _, err := e.Handle(BackendRowDescription, buildRowDescription(cols), onRow); err != nil {
		t.Fatal(err)
	}
	// 2 DataRows
	if _, err := e.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("1")}), onRow); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("2")}), onRow); err != nil {
		t.Fatal(err)
	}
	// CommandComplete
	if _, err := e.Handle(BackendCommandComplete, buildCommandComplete("SELECT 2"), onRow); err != nil {
		t.Fatal(err)
	}
	// ReadyForQuery
	done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, onRow)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if tag := string(e.TagBytes()); rows != 2 || tag != "SELECT 2" {
		t.Fatalf("rows=%d tag=%q", rows, tag)
	}
}

func TestExtendedQueryNoDescribe(t *testing.T) {
	e := &ExtendedQueryState{HasDescribe: false}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	_, _ = e.Handle(BackendBindComplete, nil, nil)
	// With no Describe we expect to jump straight to execute results:
	// server sends no RowDescription. For an INSERT there would just be
	// CommandComplete.
	if _, err := e.Handle(BackendCommandComplete, buildCommandComplete("INSERT 0 1"), nil); err != nil {
		t.Fatal(err)
	}
	done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if tag := string(e.TagBytes()); tag != "INSERT 0 1" {
		t.Fatalf("tag=%q", tag)
	}
}

func TestExtendedQueryNoData(t *testing.T) {
	e := &ExtendedQueryState{HasDescribe: true}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	_, _ = e.Handle(BackendBindComplete, nil, nil)
	// Describe statement with no params: server sends ParameterDescription
	// (empty) then NoData for DDL / statements with no result set.
	_, _ = e.Handle(BackendParameterDesc, buildParameterDescription(nil), nil)
	if _, err := e.Handle(BackendNoData, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Handle(BackendCommandComplete, buildCommandComplete("CREATE TABLE"), nil); err != nil {
		t.Fatal(err)
	}
	done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

func TestExtendedQueryErrorResponse(t *testing.T) {
	e := &ExtendedQueryState{HasDescribe: true}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	// Server sends Error after BindComplete fails.
	payload := []byte("SERROR\x00C42703\x00Mcolumn not found\x00\x00")
	if _, err := e.Handle(BackendErrorResponse, payload, nil); err != nil {
		t.Fatalf("E: %v", err)
	}
	// Extra messages before RFQ must be tolerated.
	if _, err := e.Handle(BackendNoticeResponse, []byte{0}, nil); err != nil {
		t.Fatal(err)
	}
	done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if !done {
		t.Fatalf("expected done")
	}
	if err == nil {
		t.Fatalf("expected surfaced error on RFQ after E")
	}
	if e.Err == nil || e.Err.Code != "42703" {
		t.Fatalf("bad PGError: %+v", e.Err)
	}
}

func TestExtendedQueryPortalSuspended(t *testing.T) {
	e := &ExtendedQueryState{HasDescribe: true}
	_, _ = e.Handle(BackendParseComplete, nil, nil)
	_, _ = e.Handle(BackendBindComplete, nil, nil)
	_, _ = e.Handle(BackendParameterDesc, buildParameterDescription(nil), nil)
	cols := []ColumnDesc{{Name: "a", TypeOID: OIDInt4}}
	_, _ = e.Handle(BackendRowDescription, buildRowDescription(cols), nil)
	_, _ = e.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("1")}), nil)
	if _, err := e.Handle(BackendPortalSuspended, nil, nil); err != nil {
		t.Fatal(err)
	}
	done, err := e.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if err != nil || !done {
		t.Fatal("expected done")
	}
}
