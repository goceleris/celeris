package protocol

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestReaderSimple(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("+OK\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TySimple || string(v.Str) != "OK" {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderError(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("-ERR bad\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyError || string(v.Str) != "ERR bad" {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderInt(t *testing.T) {
	cases := []struct {
		in  string
		val int64
	}{
		{":0\r\n", 0},
		{":1000\r\n", 1000},
		{":-42\r\n", -42},
	}
	for _, c := range cases {
		r := NewReader()
		r.Feed([]byte(c.in))
		v, err := r.Next()
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if v.Type != TyInt || v.Int != c.val {
			t.Fatalf("%q: got %#v", c.in, v)
		}
	}
}

func TestReaderBulk(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("$5\r\nhello\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyBulk || string(v.Str) != "hello" {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderNullBulk(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("$-1\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyNull {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderEmptyBulk(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("$0\r\n\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyBulk || len(v.Str) != 0 {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderArray(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("*3\r\n$3\r\nfoo\r\n:42\r\n+bar\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyArray || len(v.Array) != 3 {
		t.Fatalf("got %#v", v)
	}
	if string(v.Array[0].Str) != "foo" {
		t.Fatalf("arr[0] %#v", v.Array[0])
	}
	if v.Array[1].Int != 42 {
		t.Fatalf("arr[1] %#v", v.Array[1])
	}
	if string(v.Array[2].Str) != "bar" {
		t.Fatalf("arr[2] %#v", v.Array[2])
	}
	r.Release(v)
}

func TestReaderNullArray(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("*-1\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyNull {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderResp3Types(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("_\r\n#t\r\n#f\r\n,3.14\r\n(123456789012345678901234567890\r\n"))
	// null
	v, err := r.Next()
	if err != nil || v.Type != TyNull {
		t.Fatalf("null: %v %#v", err, v)
	}
	// bool true
	v, err = r.Next()
	if err != nil || v.Type != TyBool || !v.Bool {
		t.Fatalf("bool t: %v %#v", err, v)
	}
	// bool false
	v, err = r.Next()
	if err != nil || v.Type != TyBool || v.Bool {
		t.Fatalf("bool f: %v %#v", err, v)
	}
	// double
	v, err = r.Next()
	if err != nil || v.Type != TyDouble || math.Abs(v.Float-3.14) > 1e-9 {
		t.Fatalf("double: %v %#v", err, v)
	}
	// bignum
	v, err = r.Next()
	if err != nil || v.Type != TyBigInt || string(v.BigN) != "123456789012345678901234567890" {
		t.Fatalf("bigint: %v %#v", err, v)
	}
}

func TestReaderResp3Map(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("%2\r\n+a\r\n:1\r\n+b\r\n:2\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyMap || len(v.Map) != 2 {
		t.Fatalf("got %#v", v)
	}
	if string(v.Map[0].K.Str) != "a" || v.Map[0].V.Int != 1 {
		t.Fatalf("map[0] %#v", v.Map[0])
	}
	if string(v.Map[1].K.Str) != "b" || v.Map[1].V.Int != 2 {
		t.Fatalf("map[1] %#v", v.Map[1])
	}
	r.Release(v)
}

func TestReaderResp3Set(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("~2\r\n+x\r\n+y\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TySet || len(v.Array) != 2 {
		t.Fatalf("got %#v", v)
	}
	r.Release(v)
}

func TestReaderResp3Push(t *testing.T) {
	r := NewReader()
	r.Feed([]byte(">3\r\n+pubsub\r\n+subscribe\r\n+chan\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyPush || len(v.Array) != 3 {
		t.Fatalf("got %#v", v)
	}
	r.Release(v)
}

func TestReaderIncomplete(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("$5\r\nhel"))
	_, err := r.Next()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("want incomplete, got %v", err)
	}
	// Cursor should not advance.
	if r.r != 0 {
		t.Fatalf("cursor advanced: %d", r.r)
	}
	r.Feed([]byte("lo\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Str) != "hello" {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderByteByByte(t *testing.T) {
	input := []byte("*2\r\n$3\r\nfoo\r\n:42\r\n")
	r := NewReader()
	for i := 0; i < len(input)-1; i++ {
		r.Feed(input[i : i+1])
		_, err := r.Next()
		if !errors.Is(err, ErrIncomplete) {
			t.Fatalf("byte %d: want incomplete, got %v", i, err)
		}
	}
	r.Feed(input[len(input)-1:])
	v, err := r.Next()
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if v.Type != TyArray || len(v.Array) != 2 {
		t.Fatalf("got %#v", v)
	}
	r.Release(v)
}

func TestReaderCompact(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("+ok\r\n+ok2\r\n"))
	_, _ = r.Next()
	r.Compact()
	if r.r != 0 {
		t.Fatal("r not reset")
	}
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Str) != "ok2" {
		t.Fatalf("got %#v", v)
	}
}

func TestReaderLargeBulk(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), maxInlineBulk+1)
	buf := []byte("$")
	buf = append(buf, []byte("1048577")...)
	buf = append(buf, '\r', '\n')
	buf = append(buf, payload...)
	buf = append(buf, '\r', '\n')
	r := NewReader()
	r.Feed(buf)
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Str) != len(payload) {
		t.Fatalf("len %d want %d", len(v.Str), len(payload))
	}
	// Large bulks should be heap-copied (not alias r.buf).
	if &v.Str[0] == &r.buf[5+7] {
		t.Fatal("large bulk should have been copied")
	}
}

func TestWriterCommand(t *testing.T) {
	w := NewWriter()
	out := w.WriteCommand("SET", "k", "v")
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestWriterRoundTrip(t *testing.T) {
	w := NewWriter()
	out := w.WriteCommand("GET", "k")
	r := NewReader()
	r.Feed(out)
	v, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != TyArray || len(v.Array) != 2 {
		t.Fatalf("got %#v", v)
	}
	if string(v.Array[0].Str) != "GET" || string(v.Array[1].Str) != "k" {
		t.Fatalf("args mismatch: %#v", v.Array)
	}
}

func TestReaderMalformed(t *testing.T) {
	bad := []string{
		"?garbage\r\n",
		":abc\r\n",
		"#x\r\n",
		"$-2\r\n",
		"*-2\r\n",
	}
	for _, b := range bad {
		r := NewReader()
		r.Feed([]byte(b))
		_, err := r.Next()
		if err == nil || errors.Is(err, ErrIncomplete) {
			t.Fatalf("%q: want protocol error, got %v", b, err)
		}
	}
}

func BenchmarkReaderBulk(b *testing.B) {
	input := []byte("$5\r\nhello\r\n")
	r := NewReader()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Reset()
		r.Feed(input)
		v, err := r.Next()
		if err != nil {
			b.Fatal(err)
		}
		_ = v
	}
}

func BenchmarkReaderSimpleArray(b *testing.B) {
	input := []byte("*3\r\n$3\r\nfoo\r\n:42\r\n+bar\r\n")
	r := NewReader()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Reset()
		r.Feed(input)
		v, err := r.Next()
		if err != nil {
			b.Fatal(err)
		}
		r.Release(v)
	}
}

// TestReaderMaxBulkDoS guards against a hostile server advertising a bulk
// length so large it would make the Reader buffer grow without bound while
// waiting for the (never-arriving) payload. The parser must reject the
// length header immediately with ErrProtocolOversizedBulk.
func TestReaderMaxBulkDoS(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("$99999999999999999\r\n"))
	_, err := r.Next()
	if !errors.Is(err, ErrProtocolOversizedBulk) {
		t.Fatalf("want ErrProtocolOversizedBulk, got %v", err)
	}
}

// TestReaderMaxArrayDoS is the aggregate counterpart to TestReaderMaxBulkDoS.
func TestReaderMaxArrayDoS(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("*9999999999\r\n"))
	_, err := r.Next()
	if !errors.Is(err, ErrProtocolOversizedArray) {
		t.Fatalf("want ErrProtocolOversizedArray, got %v", err)
	}
}

// TestReaderMaxMapDoS is the map counterpart.
func TestReaderMaxMapDoS(t *testing.T) {
	r := NewReader()
	r.Feed([]byte("%9999999999\r\n"))
	_, err := r.Next()
	if !errors.Is(err, ErrProtocolOversizedArray) {
		t.Fatalf("want ErrProtocolOversizedArray, got %v", err)
	}
}

// TestClearPooledFlagsResetsNested verifies that ClearPooledFlags descends
// into nested aggregates so a deep-copied Value can never leak heap slices
// into the Reader's sync.Pools.
func TestClearPooledFlagsResetsNested(t *testing.T) {
	v := Value{
		Type:      TyArray,
		pooledArr: true,
		Array: []Value{
			{Type: TyArray, pooledArr: true, Array: []Value{
				{Type: TyBulk, Str: []byte("x")},
			}},
			{Type: TyMap, pooledMap: true, Map: []KV{
				{K: Value{Type: TyBulk, Str: []byte("k")}, V: Value{Type: TyBulk, Str: []byte("v")}},
			}},
		},
	}
	ClearPooledFlags(&v)
	if v.pooledArr {
		t.Fatal("top pooledArr still set")
	}
	if v.Array[0].pooledArr {
		t.Fatal("nested pooledArr still set")
	}
	if v.Array[1].pooledMap {
		t.Fatal("nested pooledMap still set")
	}
}

func TestWriterAritySpecific(t *testing.T) {
	w := NewWriter()
	// AppendCommand1
	w.Reset()
	got := string(w.AppendCommand1("PING"))
	want := "*1\r\n$4\r\nPING\r\n"
	if got != want {
		t.Fatalf("AppendCommand1: got %q want %q", got, want)
	}
	// AppendCommand2
	w.Reset()
	got = string(w.AppendCommand2("GET", "k"))
	want = "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"
	if got != want {
		t.Fatalf("AppendCommand2: got %q want %q", got, want)
	}
	// AppendCommand3
	w.Reset()
	got = string(w.AppendCommand3("SET", "k", "v"))
	want = "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	if got != want {
		t.Fatalf("AppendCommand3: got %q want %q", got, want)
	}
	// AppendCommand4
	w.Reset()
	got = string(w.AppendCommand4("SET", "k", "v", "EX"))
	want = "*4\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n"
	if got != want {
		t.Fatalf("AppendCommand4: got %q want %q", got, want)
	}
	// AppendCommand5
	w.Reset()
	got = string(w.AppendCommand5("SET", "k", "v", "EX", "60"))
	want = "*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$2\r\n60\r\n"
	if got != want {
		t.Fatalf("AppendCommand5: got %q want %q", got, want)
	}
	// Verify parity with variadic AppendCommand
	w.Reset()
	ref := string(w.AppendCommand("SET", "k", "v"))
	w.Reset()
	alt := string(w.AppendCommand3("SET", "k", "v"))
	if ref != alt {
		t.Fatalf("parity mismatch: variadic=%q arity3=%q", ref, alt)
	}
}

// TestParseUintOverflow ensures parseUint rejects values that would overflow
// int64, preventing silent wraparound on malicious RESP integer frames.
func TestParseUintOverflow(t *testing.T) {
	cases := []string{
		"99999999999999999999",    // 20 digits, way past MaxInt64
		"9223372036854775808",     // MaxInt64 + 1
		"92233720368547758070000", // many digits
	}
	for _, c := range cases {
		r := NewReader()
		r.Feed([]byte(":" + c + "\r\n"))
		_, err := r.Next()
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("parseUint(%q): want ErrProtocol, got %v", c, err)
		}
	}
	// MaxInt64 must still parse OK.
	r := NewReader()
	r.Feed([]byte(":9223372036854775807\r\n"))
	v, err := r.Next()
	if err != nil {
		t.Fatalf("MaxInt64: %v", err)
	}
	if v.Int != math.MaxInt64 {
		t.Fatalf("MaxInt64: got %d", v.Int)
	}
}

func BenchmarkWriterCommand(b *testing.B) {
	w := NewWriter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.WriteCommand("SET", "key", "value")
	}
}

func BenchmarkWriterCommand3(b *testing.B) {
	w := NewWriter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		_ = w.AppendCommand3("SET", "key", "value")
	}
}

func BenchmarkWriterCommand2(b *testing.B) {
	w := NewWriter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		_ = w.AppendCommand2("GET", "key")
	}
}
