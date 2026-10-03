package protocol

import "testing"

// BenchmarkSimpleQueryStateCycle830 drives one simple-query round trip
// through SimpleQueryState — RowDescription, one DataRow, CommandComplete,
// ReadyForQuery, then Reset — the per-query work of the driver's Query path.
// celeris#830 removed the always-empty Tag field and its dead stores from
// Handle; this is the measurement that change is judged by.
func BenchmarkSimpleQueryStateCycle830(b *testing.B) {
	rd := buildRowDescription([]ColumnDesc{{Name: "n", TypeOID: OIDInt4, TypeSize: 4, TypeModifier: -1}})
	row := buildDataRow([][]byte{[]byte("7")})
	cc := buildCommandComplete("SELECT 1")
	rfq := []byte{'I'}
	q := &SimpleQueryState{}
	b.ReportAllocs()
	for b.Loop() {
		q.Reset()
		if _, err := q.Handle(BackendRowDescription, rd, nil); err != nil {
			b.Fatal(err)
		}
		if _, err := q.Handle(BackendDataRow, row, nil); err != nil {
			b.Fatal(err)
		}
		if _, err := q.Handle(BackendCommandComplete, cc, nil); err != nil {
			b.Fatal(err)
		}
		if done, err := q.Handle(BackendReadyForQuery, rfq, nil); err != nil || !done {
			b.Fatalf("done=%v err=%v", done, err)
		}
	}
	if string(q.TagBytes()) != "SELECT 1" {
		b.Fatalf("tag = %q, want SELECT 1", q.TagBytes())
	}
}
