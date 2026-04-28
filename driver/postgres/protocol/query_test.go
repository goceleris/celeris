package protocol

import (
	"bytes"
	"testing"
)

// buildRowDescription produces a valid 'T' payload for the given columns.
func buildRowDescription(cols []ColumnDesc) []byte {
	w := NewWriter()
	w.StartMessage(BackendRowDescription)
	w.WriteInt16(int16(len(cols)))
	for _, c := range cols {
		w.WriteString(c.Name)
		w.WriteInt32(int32(c.TableOID))
		w.WriteInt16(c.ColumnAttNum)
		w.WriteInt32(int32(c.TypeOID))
		w.WriteInt16(c.TypeSize)
		w.WriteInt32(c.TypeModifier)
		w.WriteInt16(c.FormatCode)
	}
	w.FinishMessage()
	// Return only the payload (after type byte + 4 length bytes).
	return append([]byte(nil), w.Bytes()[5:]...)
}

func buildDataRow(fields [][]byte) []byte {
	w := NewWriter()
	w.StartMessage(BackendDataRow)
	w.WriteInt16(int16(len(fields)))
	for _, f := range fields {
		if f == nil {
			w.WriteInt32(-1)
			continue
		}
		w.WriteInt32(int32(len(f)))
		w.WriteBytes(f)
	}
	w.FinishMessage()
	return append([]byte(nil), w.Bytes()[5:]...)
}

func buildCommandComplete(tag string) []byte {
	w := NewWriter()
	w.StartMessage(BackendCommandComplete)
	w.WriteString(tag)
	w.FinishMessage()
	return append([]byte(nil), w.Bytes()[5:]...)
}

func TestParseRowDescription(t *testing.T) {
	cols := []ColumnDesc{
		{Name: "id", TypeOID: OIDInt4, TypeSize: 4, TypeModifier: -1, FormatCode: 0},
		{Name: "name", TypeOID: OIDText, TypeSize: -1, TypeModifier: -1, FormatCode: 0},
	}
	payload := buildRowDescription(cols)
	got, err := ParseRowDescription(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "id" || got[1].Name != "name" {
		t.Fatalf("parsed cols: %+v", got)
	}
	if got[0].TypeOID != OIDInt4 {
		t.Fatalf("type OID: %d", got[0].TypeOID)
	}
}

func TestParseDataRowNull(t *testing.T) {
	payload := buildDataRow([][]byte{[]byte("hello"), nil, []byte("x")})
	fields, err := ParseDataRow(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 {
		t.Fatalf("len=%d", len(fields))
	}
	if !bytes.Equal(fields[0], []byte("hello")) {
		t.Fatalf("fields[0]=%q", fields[0])
	}
	if fields[1] != nil {
		t.Fatalf("fields[1] should be nil, got %q", fields[1])
	}
	if !bytes.Equal(fields[2], []byte("x")) {
		t.Fatalf("fields[2]=%q", fields[2])
	}
}

func TestRowsAffected(t *testing.T) {
	cases := map[string]struct {
		n  int64
		ok bool
	}{
		"SELECT 5":     {5, true},
		"INSERT 0 3":   {3, true},
		"UPDATE 12":    {12, true},
		"DELETE 0":     {0, true},
		"CREATE TABLE": {0, false},
		"":             {0, false},
		"INSERT 0":     {0, false},
	}
	for tag, want := range cases {
		got, ok := RowsAffected(tag)
		if got != want.n || ok != want.ok {
			t.Errorf("%q: got (%d,%v), want (%d,%v)", tag, got, ok, want.n, want.ok)
		}
	}
}

func TestSimpleQueryStateFullCycle(t *testing.T) {
	cols := []ColumnDesc{{Name: "n", TypeOID: OIDInt4, TypeSize: 4, TypeModifier: -1}}
	q := &SimpleQueryState{}

	var seenRows [][][]byte
	var seenCols []ColumnDesc
	obs := HandleCallbacks{
		OnRowDescFn: func(c []ColumnDesc) { seenCols = c },
		OnRowFn: func(f [][]byte) {
			c := make([][]byte, len(f))
			for i, b := range f {
				if b != nil {
					c[i] = append([]byte(nil), b...)
				}
			}
			seenRows = append(seenRows, c)
		},
	}

	if _, err := q.Handle(BackendRowDescription, buildRowDescription(cols), obs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("1")}), obs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("2")}), obs); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Handle(BackendCommandComplete, buildCommandComplete("SELECT 2"), obs); err != nil {
		t.Fatal(err)
	}
	done, err := q.Handle(BackendReadyForQuery, []byte{'I'}, obs)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if tag := string(q.TagBytes()); tag != "SELECT 2" {
		t.Fatalf("tag=%q", tag)
	}
	if len(seenCols) != 1 || len(seenRows) != 2 {
		t.Fatalf("seen cols=%d rows=%d", len(seenCols), len(seenRows))
	}
}

func TestSimpleQueryMultiStatement(t *testing.T) {
	q := &SimpleQueryState{}
	cols1 := []ColumnDesc{{Name: "a", TypeOID: OIDInt4}}
	cols2 := []ColumnDesc{{Name: "b", TypeOID: OIDText}}
	var tags []string
	obs := HandleCallbacks{
		OnRowDescFn: func(c []ColumnDesc) {},
		OnRowFn:     func(f [][]byte) {},
	}

	// First result.
	_, _ = q.Handle(BackendRowDescription, buildRowDescription(cols1), obs)
	_, _ = q.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("1")}), obs)
	_, _ = q.Handle(BackendCommandComplete, buildCommandComplete("SELECT 1"), obs)
	tags = append(tags, string(q.TagBytes()))

	// Second result in the same Q.
	_, _ = q.Handle(BackendRowDescription, buildRowDescription(cols2), obs)
	_, _ = q.Handle(BackendDataRow, buildDataRow([][]byte{[]byte("x")}), obs)
	_, _ = q.Handle(BackendCommandComplete, buildCommandComplete("SELECT 1"), obs)
	tags = append(tags, string(q.TagBytes()))

	done, err := q.Handle(BackendReadyForQuery, []byte{'I'}, obs)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(tags) != 2 || tags[0] != "SELECT 1" || tags[1] != "SELECT 1" {
		t.Fatalf("tags=%v", tags)
	}
}

func TestSimpleQueryErrorResponse(t *testing.T) {
	q := &SimpleQueryState{}
	// Error payload: S=ERROR, C=42601, M=syntax error.
	payload := []byte("SERROR\x00C42601\x00Msyntax error\x00\x00")
	if _, err := q.Handle(BackendErrorResponse, payload, nil); err != nil {
		t.Fatalf("Handle(E): %v", err)
	}
	if q.Err == nil || q.Err.Code != "42601" {
		t.Fatalf("bad PGError: %+v", q.Err)
	}
	done, err := q.Handle(BackendReadyForQuery, []byte{'I'}, nil)
	if !done {
		t.Fatalf("expected done after RFQ")
	}
	if err == nil {
		t.Fatalf("expected surfaced error on RFQ after E")
	}
}

func TestWriteQuery(t *testing.T) {
	w := NewWriter()
	msg := WriteQuery(w, "SELECT 1")
	if msg[0] != MsgQuery {
		t.Fatalf("type=%q", msg[0])
	}
	// payload after 5-byte header should be "SELECT 1\x00".
	if string(msg[5:]) != "SELECT 1\x00" {
		t.Fatalf("payload=%q", msg[5:])
	}
}

func TestParseErrorResponse(t *testing.T) {
	payload := []byte("SERROR\x00C28P01\x00Minvalid password\x00Dthe detail\x00H\x00P42\x00\x00")
	e := ParseErrorResponse(payload)
	if e.Severity != "ERROR" || e.Code != "28P01" || e.Message != "invalid password" {
		t.Fatalf("parsed: %+v", e)
	}
	if e.Position != 42 {
		t.Fatalf("position=%d", e.Position)
	}
	if e.Detail != "the detail" {
		t.Fatalf("detail=%q", e.Detail)
	}
	s := e.Error()
	if !bytes.Contains([]byte(s), []byte("SQLSTATE 28P01")) {
		t.Fatalf("Error(): %s", s)
	}
}
