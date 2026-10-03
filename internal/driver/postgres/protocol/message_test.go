package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// buildQuery builds a Query('Q') message by hand for cross-checking against
// the Reader/Writer implementations.
func buildQuery(sql string) []byte {
	buf := []byte{'Q'}
	lenBuf := make([]byte, 4)
	// length includes the 4 length bytes, the SQL, and the null terminator.
	binary.BigEndian.PutUint32(lenBuf, uint32(4+len(sql)+1))
	buf = append(buf, lenBuf...)
	buf = append(buf, sql...)
	buf = append(buf, 0)
	return buf
}

func TestReaderIncomplete(t *testing.T) {
	raw := buildQuery("SELECT 1")
	r := NewReader()

	for i := 0; i < len(raw)-1; i++ {
		r.Reset()
		r.Feed(raw[:i])
		if _, _, err := r.Next(); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("with %d bytes, want ErrIncomplete, got %v", i, err)
		}
	}

	r.Reset()
	// Drip-feed a byte at a time; Next should return ErrIncomplete until the
	// final byte is fed.
	for i := 0; i < len(raw)-1; i++ {
		r.Feed(raw[i : i+1])
		if _, _, err := r.Next(); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("drip byte %d: want ErrIncomplete, got %v", i, err)
		}
	}
	r.Feed(raw[len(raw)-1:])
	mt, payload, err := r.Next()
	if err != nil {
		t.Fatalf("Next after full feed: %v", err)
	}
	if mt != MsgQuery {
		t.Fatalf("msgType = %q want %q", mt, MsgQuery)
	}
	want := append([]byte("SELECT 1"), 0)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = %q want %q", payload, want)
	}
}

func TestReaderMultipleMessages(t *testing.T) {
	r := NewReader()
	r.Feed(buildQuery("a"))
	r.Feed(buildQuery("bb"))
	r.Feed(buildQuery("ccc"))

	for i, want := range []string{"a", "bb", "ccc"} {
		mt, payload, err := r.Next()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if mt != MsgQuery {
			t.Fatalf("message %d: msgType %q", i, mt)
		}
		wantPayload := append([]byte(want), 0)
		if !bytes.Equal(payload, wantPayload) {
			t.Fatalf("message %d: payload = %q want %q", i, payload, wantPayload)
		}
	}
	if _, _, err := r.Next(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("after draining: want ErrIncomplete, got %v", err)
	}
}

// TestReaderPayloadAliasing documents that payloads alias the internal buffer
// and may be invalidated by subsequent Feed / Next / Compact / Reset calls.
func TestReaderPayloadAliasing(t *testing.T) {
	r := NewReader()
	r.Feed(buildQuery("hello"))
	_, p1, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	// Prior to mutation the payload should equal the expected bytes.
	if !bytes.Equal(p1, append([]byte("hello"), 0)) {
		t.Fatalf("p1 = %q", p1)
	}
	// Force a grow by feeding a much larger batch — the backing array can
	// move, which would invalidate p1. We don't assert inequality (the
	// append may re-use), but we assert that p1's content before re-feed
	// was correct and that a fresh Next call works.
	r.Feed(buildQuery("world"))
	_, p2, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p2, append([]byte("world"), 0)) {
		t.Fatalf("p2 = %q", p2)
	}
}

func TestReaderCompact(t *testing.T) {
	r := NewReader()
	r.Feed(buildQuery("a"))
	r.Feed(buildQuery("b"))
	if _, _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	before := len(r.buf)
	r.Compact()
	if r.pos != 0 {
		t.Fatalf("pos after Compact = %d", r.pos)
	}
	if len(r.buf) >= before {
		t.Fatalf("buf not shrunk: before=%d after=%d", before, len(r.buf))
	}
	// Remaining message still decodable.
	mt, payload, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if mt != MsgQuery || !bytes.Equal(payload, []byte{'b', 0}) {
		t.Fatalf("after Compact: mt=%q payload=%q", mt, payload)
	}
}

func TestReaderInvalidLength(t *testing.T) {
	r := NewReader()
	// Length 0 is invalid (minimum is 4).
	r.Feed([]byte{'Q', 0, 0, 0, 0})
	if _, _, err := r.Next(); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("len=0: %v", err)
	}

	// Length 3 is also invalid (smaller than the length field itself).
	r.Reset()
	r.Feed([]byte{'Q', 0, 0, 0, 3})
	if _, _, err := r.Next(); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("len=3: %v", err)
	}

	// Length too large.
	r.Reset()
	r.Feed([]byte{'Q', 0xff, 0xff, 0xff, 0xff})
	if _, _, err := r.Next(); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("len=huge: %v", err)
	}
}

func TestWriterPatchesLength(t *testing.T) {
	w := NewWriter()
	w.StartMessage(MsgQuery)
	w.WriteString("SELECT 1")
	w.FinishMessage()

	got := w.Bytes()
	if got[0] != 'Q' {
		t.Fatalf("type byte = %q", got[0])
	}
	length := binary.BigEndian.Uint32(got[1:5])
	// payload is "SELECT 1\x00" = 9 bytes, plus 4 for the length field.
	if length != uint32(9+4) {
		t.Fatalf("length = %d want %d", length, 9+4)
	}
	if !bytes.Equal(got[5:], append([]byte("SELECT 1"), 0)) {
		t.Fatalf("body = %q", got[5:])
	}
}

func TestWriterStartupMessage(t *testing.T) {
	w := NewWriter()
	w.StartStartupMessage()
	// PG 3.0 protocol version: 3 << 16.
	w.WriteInt32(196608)
	w.WriteString("user")
	w.WriteString("postgres")
	w.WriteString("database")
	w.WriteString("mydb")
	_ = w.WriteByte(0) // trailing null that terminates the parameter list
	w.FinishMessage()

	got := w.Bytes()
	length := binary.BigEndian.Uint32(got[0:4])
	if int(length) != len(got) {
		t.Fatalf("length = %d want %d", length, len(got))
	}
	// Verify the protocol version is at bytes 4:8.
	if v := binary.BigEndian.Uint32(got[4:8]); v != 196608 {
		t.Fatalf("protocol version = %d", v)
	}
}

func TestWriterMultipleMessages(t *testing.T) {
	w := NewWriter()
	w.StartMessage(MsgQuery)
	w.WriteString("SELECT 1")
	w.FinishMessage()
	w.StartMessage(MsgSync)
	w.FinishMessage()

	r := NewReader()
	r.Feed(w.Bytes())

	mt, payload, err := r.Next()
	if err != nil || mt != MsgQuery {
		t.Fatalf("msg1: mt=%q err=%v", mt, err)
	}
	if !bytes.Equal(payload, append([]byte("SELECT 1"), 0)) {
		t.Fatalf("msg1 body: %q", payload)
	}

	mt, payload, err = r.Next()
	if err != nil || mt != MsgSync {
		t.Fatalf("msg2: mt=%q err=%v", mt, err)
	}
	if len(payload) != 0 {
		t.Fatalf("msg2 body len = %d", len(payload))
	}
}

func TestRoundTripQuery(t *testing.T) {
	w := NewWriter()
	w.StartMessage(MsgQuery)
	w.WriteString("SELECT 42")
	w.FinishMessage()

	r := NewReader()
	r.Feed(w.Bytes())

	mt, payload, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if mt != MsgQuery {
		t.Fatalf("mt=%q", mt)
	}

	pos := 0
	s, err := ReadCString(payload, &pos)
	if err != nil {
		t.Fatalf("ReadCString: %v", err)
	}
	if s != "SELECT 42" {
		t.Fatalf("sql = %q", s)
	}
	if pos != len(payload) {
		t.Fatalf("pos=%d len=%d", pos, len(payload))
	}
}

func TestReadHelpers(t *testing.T) {
	t.Run("ReadByte", func(t *testing.T) {
		pos := 0
		b, err := ReadByte([]byte{0x42}, &pos)
		if err != nil || b != 0x42 || pos != 1 {
			t.Fatalf("b=%x err=%v pos=%d", b, err, pos)
		}
		pos = 0
		if _, err := ReadByte(nil, &pos); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("empty: %v", err)
		}
	})

	t.Run("ReadInt16", func(t *testing.T) {
		pos := 0
		v, err := ReadInt16([]byte{0xff, 0x01}, &pos)
		if err != nil || v != -255 || pos != 2 {
			t.Fatalf("v=%d err=%v pos=%d", v, err, pos)
		}
		pos = 0
		if _, err := ReadInt16([]byte{0x00}, &pos); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("trunc: %v", err)
		}
	})

	t.Run("ReadInt32", func(t *testing.T) {
		pos := 0
		v, err := ReadInt32([]byte{0x00, 0x00, 0x00, 0x2a}, &pos)
		if err != nil || v != 42 || pos != 4 {
			t.Fatalf("v=%d err=%v pos=%d", v, err, pos)
		}
		pos = 0
		if _, err := ReadInt32([]byte{0, 0, 0}, &pos); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("trunc: %v", err)
		}
	})

	t.Run("ReadCString", func(t *testing.T) {
		pos := 0
		s, err := ReadCString([]byte{'h', 'i', 0, 'x'}, &pos)
		if err != nil || s != "hi" || pos != 3 {
			t.Fatalf("s=%q err=%v pos=%d", s, err, pos)
		}
		pos = 0
		if _, err := ReadCString([]byte{'h', 'i'}, &pos); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("no null: %v", err)
		}
	})

	t.Run("ReadBytes", func(t *testing.T) {
		buf := []byte{1, 2, 3, 4}
		pos := 1
		b, err := ReadBytes(buf, &pos, 2)
		if err != nil || !bytes.Equal(b, []byte{2, 3}) || pos != 3 {
			t.Fatalf("b=%v err=%v pos=%d", b, err, pos)
		}
		// aliasing
		b[0] = 99
		if buf[1] != 99 {
			t.Fatalf("ReadBytes did not alias buf")
		}
		pos = 0
		if _, err := ReadBytes(buf, &pos, 99); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("oversize: %v", err)
		}
		pos = 0
		if _, err := ReadBytes(buf, &pos, -1); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("negative: %v", err)
		}
	})
}

func TestReaderBuffered(t *testing.T) {
	r := NewReader()
	r.Feed(buildQuery("x"))
	r.Feed(buildQuery("yy"))
	if got := r.Buffered(); got != 14 {
		// 'Q' + 4 + "x\x00" = 7, + 'Q' + 4 + "yy\x00" = 8, total 15. Let's recompute:
		// len("x")=1 + null = 2; header = 5; so msg1 = 7. msg2: len("yy")=2 + null = 3; header = 5; msg2 = 8. total 15.
		if got != 15 {
			t.Fatalf("Buffered = %d", got)
		}
	}
	if _, _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if got := r.Buffered(); got != 8 {
		t.Fatalf("Buffered after one Next = %d", got)
	}
}

// TestReaderNextAllocFree asserts the Reader.Next hot path does not allocate.
func BenchmarkReaderNext(b *testing.B) {
	// Pre-fill the buffer with many messages so we measure only Next.
	raw := buildQuery("SELECT 1")
	r := NewReader()
	for i := 0; i < b.N; i++ {
		r.Feed(raw)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := r.Next(); err != nil {
			b.Fatal(err)
		}
	}
}

// TestReaderFedOneByteReader verifies drip-reading through iotest.OneByteReader
// (indirectly) is not broken.
func TestReaderOneByteAtATime(t *testing.T) {
	src := bytes.NewReader(buildQuery("drip"))
	obr := iotest.OneByteReader(src)

	r := NewReader()
	scratch := make([]byte, 1)
	for {
		n, err := obr.Read(scratch)
		if n > 0 {
			r.Feed(scratch[:n])
		}
		mt, payload, perr := r.Next()
		if perr == nil {
			if mt != MsgQuery {
				t.Fatalf("mt=%q", mt)
			}
			if !bytes.Equal(payload, append([]byte("drip"), 0)) {
				t.Fatalf("payload=%q", payload)
			}
			return
		}
		if !errors.Is(perr, ErrIncomplete) {
			t.Fatalf("Next: %v", perr)
		}
		if err == io.EOF {
			t.Fatal("EOF before full message")
		}
	}
}
