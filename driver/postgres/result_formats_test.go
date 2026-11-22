package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// rfColumn is one result column of the format-aware fake server: its type and
// the bytes it sends for the value in the binary and in the text format.
type rfColumn struct {
	name string
	oid  uint32
	bin  []byte
	text []byte
}

// rfServer is a fake PostgreSQL extended-protocol server that behaves like the
// real one where result formats are concerned: it records the result-format
// codes of every Bind, answers Describe 'S' with format 0 (the format is not
// decided yet) and Describe 'P' and the DataRows with the format Bind chose
// (zero codes: all text, one code: all columns, N codes: per column).
type rfServer struct {
	cols []rfColumn

	mu    sync.Mutex
	binds []rfBind
}

type rfBind struct {
	stmt    string
	formats []int16
}

func (s *rfServer) recorded() []rfBind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rfBind(nil), s.binds...)
}

func rfParseBind(p []byte) rfBind {
	i := 0
	cstr := func() string {
		j := i
		for p[j] != 0 {
			j++
		}
		s := string(p[i:j])
		i = j + 1
		return s
	}
	i16 := func() int {
		v := int(binary.BigEndian.Uint16(p[i:]))
		i += 2
		return v
	}
	_ = cstr() // portal
	b := rfBind{stmt: cstr()}
	for n := i16(); n > 0; n-- { // parameter formats
		i += 2
	}
	for n := i16(); n > 0; n-- { // parameters
		l := int32(binary.BigEndian.Uint32(p[i:]))
		i += 4
		if l > 0 {
			i += int(l)
		}
	}
	for n := i16(); n > 0; n-- {
		b.formats = append(b.formats, int16(i16()))
	}
	return b
}

func (s *rfServer) formatOf(formats []int16, col int) int16 {
	switch len(formats) {
	case 0:
		return protocol.FormatText
	case 1:
		return formats[0]
	}
	return formats[col]
}

func (s *rfServer) handler(tb testing.TB) func(net.Conn) {
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			var out []byte
			put := func(typ byte, payload []byte) {
				msg := make([]byte, 5+len(payload))
				msg[0] = typ
				binary.BigEndian.PutUint32(msg[1:5], uint32(4+len(payload)))
				copy(msg[5:], payload)
				out = append(out, msg...)
			}
			rowDesc := func(formats []int16) []byte {
				specs := make([]colSpec, len(s.cols))
				for i, col := range s.cols {
					specs[i] = colSpec{name: col.name, typeOID: col.oid, typeSize: -1, format: s.formatOf(formats, i)}
				}
				return buildRowDescription(specs)
			}
			var last []int16
			for {
				typ, payload, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgParse:
					put(protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					if payload[0] == 'S' {
						put(protocol.BackendParameterDesc, buildParameterDescription(nil))
						put(protocol.BackendRowDescription, rowDesc(nil))
					} else {
						put(protocol.BackendRowDescription, rowDesc(last))
					}
				case protocol.MsgBind:
					b := rfParseBind(payload)
					last = b.formats
					s.mu.Lock()
					s.binds = append(s.binds, b)
					s.mu.Unlock()
					put(protocol.BackendBindComplete, nil)
				case protocol.MsgExecute:
					fields := make([][]byte, len(s.cols))
					for i, col := range s.cols {
						if s.formatOf(last, i) == protocol.FormatBinary {
							fields[i] = col.bin
						} else {
							fields[i] = col.text
						}
					}
					put(protocol.BackendDataRow, buildDataRow(fields))
					put(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0))
				case protocol.MsgSync:
					put(protocol.BackendReadyForQuery, []byte{'I'})
					_, _ = c.Write(out)
					out = out[:0]
				case protocol.MsgQuery:
					put(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))
					put(protocol.BackendReadyForQuery, []byte{'I'})
					_, _ = c.Write(out)
					out = out[:0]
				case protocol.MsgClose:
					put(protocol.BackendCloseComplete, nil)
				}
			}
		})
	}
}

func rfBinInt4(n int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(n))
	return b
}

// rfInterval1Day is interval '1 day' on the wire: int64 microseconds, int32
// days, int32 months.
func rfInterval1Day() []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b[8:], 1)
	return b
}

func rfDial(tb testing.TB, s *rfServer, autoCache bool) *pgConn {
	tb.Helper()
	addr := startFakePGBench(tb, s.handler(tb))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { eventloop.Release(prov) })
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{
		SSLMode: "disable", StatementCacheSize: 16, AutoCacheStatements: autoCache,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = c.Close() })
	return c
}

func rfFormats(binds []rfBind) [][]int16 {
	var out [][]int16
	for _, b := range binds {
		out = append(out, b.formats)
	}
	return out
}

// rfNext reads the single row and returns its values.
func rfNext(tb testing.TB, rows driver.Rows, n int) []driver.Value {
	tb.Helper()
	defer func() { _ = rows.Close() }()
	dest := make([]driver.Value, n)
	if err := rows.Next(dest); err != nil {
		tb.Fatalf("Next: %v", err)
	}
	return dest
}

func rfMixed() *rfServer {
	return &rfServer{cols: []rfColumn{
		{"id", protocol.OIDInt4, rfBinInt4(7), []byte("7")},
		{"name", protocol.OIDText, []byte("bob"), []byte("bob")},
		{"gap", 1186, rfInterval1Day(), []byte("1 day")}, // interval: no codec
		{"score", protocol.OIDInt4, rfBinInt4(9), []byte("9")},
	}}
}

// A statement the driver caches asks for binary only for the columns it has a
// codec for and for text for the others, and the columns decode accordingly.
func TestResultFormats_CachedStatementPerColumn(t *testing.T) {
	s := rfMixed()
	c := rfDial(t, s, true)
	ctx := context.Background()
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}
	for i := 0; i < 3; i++ { // miss, then hits
		rows, err := c.QueryContext(ctx, "SELECT id, name, gap, score FROM t WHERE id = $1", args)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		got := rfNext(t, rows, 4)
		want := []driver.Value{int64(7), "bob", []byte("1 day"), int64(9)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: got %#v, want %#v", i, got, want)
		}
	}
	wantFmt := []int16{protocol.FormatBinary, protocol.FormatBinary, protocol.FormatText, protocol.FormatBinary}
	binds := s.recorded()
	if len(binds) != 3 {
		t.Fatalf("binds = %v", rfFormats(binds))
	}
	for i, b := range binds {
		if !reflect.DeepEqual(b.formats, wantFmt) {
			t.Errorf("bind %d: result formats %v, want %v", i, b.formats, wantFmt)
		}
	}
}

// When every column has a codec the Bind result-format vector stays the single
// binary code it has always been (the bytes on the wire do not change).
func TestResultFormats_CachedStatementAllBinaryKeepsSingleCode(t *testing.T) {
	s := &rfServer{cols: []rfColumn{
		{"id", protocol.OIDInt4, rfBinInt4(7), []byte("7")},
		{"name", protocol.OIDText, []byte("bob"), []byte("bob")},
		{"email", protocol.OIDText, []byte("b@x"), []byte("b@x")},
		{"score", protocol.OIDInt4, rfBinInt4(9), []byte("9")},
	}}
	c := rfDial(t, s, true)
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}
	for i := 0; i < 2; i++ {
		rows, err := c.QueryContext(context.Background(), "SELECT id, name, email, score FROM t WHERE id = $1", args)
		if err != nil {
			t.Fatal(err)
		}
		got := rfNext(t, rows, 4)
		want := []driver.Value{int64(7), "bob", "b@x", int64(9)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	for i, b := range s.recorded() {
		if !reflect.DeepEqual(b.formats, []int16{protocol.FormatBinary}) {
			t.Errorf("bind %d: result formats %v, want [1]", i, b.formats)
		}
	}
}

// An explicit Prepare knows its columns from the prepare-time Describe.
func TestResultFormats_ExplicitPrepare(t *testing.T) {
	s := rfMixed()
	c := rfDial(t, s, true)
	ctx := context.Background()
	st, err := c.PrepareContext(ctx, "SELECT id, name, gap, score FROM t WHERE id = $1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rows, err := st.(*pgStmt).QueryContext(ctx, []driver.NamedValue{{Ordinal: 1, Value: int64(7)}})
		if err != nil {
			t.Fatal(err)
		}
		got := rfNext(t, rows, 4)
		want := []driver.Value{int64(7), "bob", []byte("1 day"), int64(9)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	wantFmt := []int16{protocol.FormatBinary, protocol.FormatBinary, protocol.FormatText, protocol.FormatBinary}
	for i, b := range s.recorded() {
		if !reflect.DeepEqual(b.formats, wantFmt) {
			t.Errorf("bind %d: result formats %v, want %v", i, b.formats, wantFmt)
		}
	}
}

// With no cached statement the columns are unknown when Bind is sent (Parse,
// Bind, Describe, Execute go out together), so the driver asks for text for all
// of them rather than risk binary bytes it cannot decode.
func TestResultFormats_UnnamedStatementIsText(t *testing.T) {
	s := rfMixed()
	c := rfDial(t, s, false)
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}
	rows, err := c.QueryContext(context.Background(), "SELECT id, name, gap, score FROM t WHERE id = $1", args)
	if err != nil {
		t.Fatal(err)
	}
	got := rfNext(t, rows, 4)
	want := []driver.Value{int64(7), "bob", []byte("1 day"), int64(9)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	binds := s.recorded()
	if len(binds) != 1 || !reflect.DeepEqual(binds[0].formats, []int16{protocol.FormatText}) {
		t.Fatalf("result formats = %v, want [[0]]", rfFormats(binds))
	}
}

// benchExtendedRow4Col measures one extended-protocol query returning a single
// int/text/text/int row (the shape of the probatorium driver-pg-read cells)
// from Bind to the decoded driver.Values.
func benchExtendedRow4Col(b *testing.B, autoCache bool) {
	s := &rfServer{cols: []rfColumn{
		{"id", protocol.OIDInt4, rfBinInt4(7), []byte("7")},
		{"name", protocol.OIDText, []byte("user-7"), []byte("user-7")},
		{"email", protocol.OIDText, []byte("user-7@example.com"), []byte("user-7@example.com")},
		{"score", protocol.OIDInt4, rfBinInt4(70), []byte("70")},
	}}
	c := rfDial(b, s, autoCache)
	ctx := context.Background()
	args := []driver.NamedValue{{Ordinal: 1, Value: int64(7)}}
	dest := make([]driver.Value, 4)
	const q = "SELECT id, name, email, score FROM t WHERE id = $1"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := c.QueryContext(ctx, q, args)
		if err != nil {
			b.Fatal(err)
		}
		if err := rows.Next(dest); err != nil {
			b.Fatal(err)
		}
		_ = rows.Close()
	}
	b.StopTimer()
	if n, _ := dest[0].(int64); n != 7 {
		b.Fatalf("id = %v", dest[0])
	}
}

func BenchmarkExtendedRow4ColCached(b *testing.B)  { benchExtendedRow4Col(b, true) }
func BenchmarkExtendedRow4ColUnnamed(b *testing.B) { benchExtendedRow4Col(b, false) }
