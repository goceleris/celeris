//go:build redisspec

package redisspec

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// FuzzRESPParse feeds random bytes to protocol.Reader.Next and verifies no
// panics occur. This exercises the parser's error-handling paths with
// arbitrary input.
func FuzzRESPParse(f *testing.F) {
	seeds := [][]byte{
		[]byte("+OK\r\n"),
		[]byte("-ERR bad\r\n"),
		[]byte(":42\r\n"),
		[]byte("$3\r\nfoo\r\n"),
		[]byte("*2\r\n$3\r\nfoo\r\n:1\r\n"),
		[]byte("$-1\r\n"),
		[]byte("*-1\r\n"),
		[]byte("*0\r\n"),
		[]byte("$0\r\n\r\n"),
		[]byte("_\r\n"),
		[]byte("#t\r\n"),
		[]byte("#f\r\n"),
		[]byte(",3.14\r\n"),
		[]byte("(12345\r\n"),
		[]byte("!6\r\nSYNTAX\r\n"),
		[]byte("=10\r\ntxt:hello\r\n"),
		[]byte("~2\r\n+a\r\n+b\r\n"),
		[]byte("%1\r\n+k\r\n+v\r\n"),
		[]byte("|1\r\n+k\r\n+v\r\n"),
		[]byte(">2\r\n+push\r\n+data\r\n"),
		[]byte("?\r\n"),                   // invalid tag
		[]byte("$999999999999999999\r\n"),  // oversized bulk
		[]byte("*999999999999999999\r\n"),  // oversized array
		[]byte("$5\r\nhel"),               // incomplete
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := protocol.NewReader()
		r.Feed(data)
		for i := 0; i < 128; i++ {
			v, err := r.Next()
			if err != nil {
				return
			}
			r.Release(v)
		}
	})
}

// FuzzRESPRoundTrip generates valid RESP command frames via Writer, parses
// them back via Reader, and verifies fidelity.
func FuzzRESPRoundTrip(f *testing.F) {
	f.Add("PING", "", "")
	f.Add("SET", "key", "value")
	f.Add("GET", "key", "")
	f.Add("MGET", "k1", "k2")
	f.Add("SET", "k", "\x00\r\n")
	f.Add("SET", "k", "")
	f.Add("SET", "", "")

	f.Fuzz(func(t *testing.T, a0, a1, a2 string) {
		var args []string
		args = append(args, a0)
		if a1 != "" {
			args = append(args, a1)
		}
		if a2 != "" {
			args = append(args, a2)
		}
		if len(args) == 0 {
			return
		}

		w := protocol.NewWriter()
		w.AppendCommand(args...)

		r := protocol.NewReader()
		r.Feed(w.Bytes())
		v, err := r.Next()
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if v.Type != protocol.TyArray {
			t.Fatalf("expected array, got %v", v.Type)
		}
		if len(v.Array) != len(args) {
			t.Fatalf("expected %d args, got %d", len(args), len(v.Array))
		}
		for i, a := range args {
			if v.Array[i].Type != protocol.TyBulk {
				t.Fatalf("arg[%d]: expected bulk, got %v", i, v.Array[i].Type)
			}
			if string(v.Array[i].Str) != a {
				t.Fatalf("arg[%d]: expected %q, got %q", i, a, v.Array[i].Str)
			}
		}
		r.Release(v)
	})
}

// FuzzRESP3Types generates RESP3-typed frames with random payloads and
// verifies the parser handles them without panics or data corruption.
func FuzzRESP3Types(f *testing.F) {
	f.Add(byte(0), int64(0))
	f.Add(byte(1), int64(1))
	f.Add(byte(2), int64(42))
	f.Add(byte(3), int64(-1))
	f.Add(byte(4), int64(314))
	f.Add(byte(5), int64(0))

	f.Fuzz(func(t *testing.T, typeIdx byte, val int64) {
		var frame []byte
		switch typeIdx % 6 {
		case 0: // null
			frame = []byte("_\r\n")
		case 1: // bool
			if val%2 == 0 {
				frame = []byte("#t\r\n")
			} else {
				frame = []byte("#f\r\n")
			}
		case 2: // integer
			frame = []byte(":" + strconv.FormatInt(val, 10) + "\r\n")
		case 3: // double
			f64 := math.Float64frombits(uint64(val))
			if math.IsNaN(f64) {
				frame = []byte(",nan\r\n")
			} else if math.IsInf(f64, 1) {
				frame = []byte(",inf\r\n")
			} else if math.IsInf(f64, -1) {
				frame = []byte(",-inf\r\n")
			} else {
				frame = []byte("," + strconv.FormatFloat(f64, 'f', -1, 64) + "\r\n")
			}
		case 4: // bignum
			frame = []byte("(" + strconv.FormatInt(val, 10) + "\r\n")
		case 5: // simple string
			s := strconv.FormatInt(val, 10)
			frame = []byte("+" + s + "\r\n")
		}

		r := protocol.NewReader()
		r.Feed(frame)
		v, err := r.Next()
		if err != nil {
			return
		}
		r.Release(v)
	})
}

// FuzzBulkBoundary tests bulk strings at various length boundaries to
// exercise buffer management edge cases.
func FuzzBulkBoundary(f *testing.F) {
	f.Add(uint16(0))
	f.Add(uint16(1))
	f.Add(uint16(127))
	f.Add(uint16(128))
	f.Add(uint16(255))
	f.Add(uint16(256))
	f.Add(uint16(1023))
	f.Add(uint16(1024))
	f.Add(uint16(4095))
	f.Add(uint16(4096))
	f.Add(uint16(32767))
	f.Add(uint16(65535))

	f.Fuzz(func(t *testing.T, size uint16) {
		n := int(size)
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i % 256)
		}

		// Build $N\r\n<payload>\r\n
		frame := fmt.Appendf(nil, "$%d\r\n", n)
		frame = append(frame, payload...)
		frame = append(frame, '\r', '\n')

		r := protocol.NewReader()
		r.Feed(frame)
		v, err := r.Next()
		if err != nil {
			t.Fatalf("parse error for size %d: %v", n, err)
		}
		if v.Type != protocol.TyBulk {
			t.Fatalf("expected bulk, got %v", v.Type)
		}
		if len(v.Str) != n {
			t.Fatalf("bulk length %d, want %d", len(v.Str), n)
		}
		for i := range payload {
			if v.Str[i] != payload[i] {
				t.Fatalf("byte %d: got 0x%02x, want 0x%02x", i, v.Str[i], payload[i])
			}
		}
		r.Release(v)
	})
}
