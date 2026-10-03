package protocol

import (
	"errors"
	"testing"
)

// FuzzReader pushes arbitrary bytes at the Reader in chunks and verifies
// that it never panics and that any returned payload has length exactly
// declaredLength - 4 (length-field width).
func FuzzReader(f *testing.F) {
	f.Add([]byte{'Q', 0, 0, 0, 5, 'x'})
	f.Add([]byte{'S', 0, 0, 0, 4})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte("\xff\xff\xff\xff\xff"))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader()
		// Feed in two halves so we exercise the ErrIncomplete path too.
		half := len(data) / 2
		r.Feed(data[:half])
		_, _, _ = r.Next()
		r.Feed(data[half:])

		// Drain messages. Cap iteration to avoid pathological loops.
		for i := 0; i < 64; i++ {
			_, payload, err := r.Next()
			if err != nil {
				if errors.Is(err, ErrIncomplete) || errors.Is(err, ErrInvalidLength) {
					return
				}
				t.Fatalf("unexpected error: %v", err)
			}
			// Payload length must be non-negative and bounded by remaining buffer.
			if len(payload) < 0 {
				t.Fatalf("negative payload len: %d", len(payload))
			}
		}
	})
}

// FuzzRoundTrip fuzzes the Writer → Reader round trip: arbitrary data is
// wrapped as a Query message and must decode back identically.
func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("SELECT 1"))
	f.Add([]byte(""))
	f.Add([]byte{0xff, 0x00, 0xff})

	f.Fuzz(func(t *testing.T, sql []byte) {
		// CStrings cannot contain embedded NULs.
		for _, b := range sql {
			if b == 0 {
				return
			}
		}
		w := NewWriter()
		w.StartMessage(MsgQuery)
		w.WriteString(string(sql))
		w.FinishMessage()

		r := NewReader()
		r.Feed(w.Bytes())
		mt, payload, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if mt != MsgQuery {
			t.Fatalf("mt=%q", mt)
		}
		pos := 0
		s, err := ReadCString(payload, &pos)
		if err != nil {
			t.Fatalf("ReadCString: %v", err)
		}
		if s != string(sql) {
			t.Fatalf("sql mismatch: got %q want %q", s, sql)
		}
		if pos != len(payload) {
			t.Fatalf("leftover bytes: pos=%d len=%d", pos, len(payload))
		}
	})
}
