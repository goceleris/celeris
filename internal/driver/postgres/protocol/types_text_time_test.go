package protocol

import (
	"encoding/binary"
	"testing"
)

func i64(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func TestDecodeTimeBinary(t *testing.T) {
	for _, c := range []struct {
		us   int64
		want string
	}{
		{0, "00:00:00"},
		{1, "00:00:00.000001"},
		{100000, "00:00:00.1"},
		{1500000, "00:00:01.5"},
		{59*usecsPerSec + 999999, "00:00:59.999999"},
		{12*usecsPerHour + 34*usecsPerMinute + 56*usecsPerSec + 400000, "12:34:56.4"},
		{86399999999, "23:59:59.999999"},
		{86400000000, "24:00:00"},
	} {
		v, err := LookupOID(OIDTime).DecodeBinary(i64(c.us))
		if err != nil || v != c.want {
			t.Errorf("%d: got %v err %v, want %q", c.us, v, err, c.want)
		}
	}
	for _, bad := range [][]byte{i64(-1), i64(86400000001), make([]byte, 7), nil} {
		if v, err := decodeTimeBinary(bad); err == nil {
			t.Errorf("% x: got %v, want an error", bad, v)
		}
	}
	if LookupOID(OIDTime).DecodeText != nil {
		t.Error("time has no text decoder: the simple protocol's text is returned as it is")
	}
}
