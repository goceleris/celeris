package protocol

import (
	"encoding/binary"
	"math"
	"testing"
)

func intervalBin(us int64, day, mon int32) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, uint64(us))
	binary.BigEndian.PutUint32(b[8:], uint32(day))
	binary.BigEndian.PutUint32(b[12:], uint32(mon))
	return b
}

// The expected text is what PostgreSQL prints with IntervalStyle = postgres.
func TestDecodeIntervalBinary(t *testing.T) {
	const h, m, s = usecsPerHour, usecsPerMinute, usecsPerSec
	for _, c := range []struct {
		us       int64
		day, mon int32
		want     string
	}{
		{0, 0, 0, "00:00:00"},
		{0, 0, 1, "1 mon"},
		{0, 0, -1, "-1 mons"},
		{0, 1, 0, "1 day"},
		{0, -1, 0, "-1 days"},
		{0, 0, 12, "1 year"},
		{0, 0, 13, "1 year 1 mon"},
		{0, 0, -13, "-1 years -1 mons"},
		{0, 0, 25, "2 years 1 mon"},
		{1, 0, 0, "00:00:00.000001"},
		{-1, 0, 0, "-00:00:00.000001"},
		{100 * h, 0, 0, "100:00:00"},
		{-100 * h, 0, 0, "-100:00:00"},
		{4*h + 5*m + 6*s + 789000, 3, 14, "1 year 2 mons 3 days 04:05:06.789"},
		{-(4*h + 5*m + 6*s + 789000), -3, -14, "-1 years -2 mons -3 days -04:05:06.789"},
		{-(4*h + 5*m + 6*s), 3, 0, "3 days -04:05:06"},
		{4*h + 5*m + 6*s, -3, 0, "-3 days +04:05:06"},
		{s, 0, 12, "1 year 00:00:01"},
		{1, -1, 1, "1 mon -1 days +00:00:00.000001"},
		{-1, 1, -1, "-1 mons +1 day -00:00:00.000001"},
		{0, 1, -1, "-1 mons +1 day"},
		{math.MaxInt64, math.MaxInt32 - 1, math.MaxInt32 - 1, "178956970 years 6 mons 2147483646 days 2562047788:00:54.775807"},
		{math.MinInt64, math.MinInt32 + 1, math.MinInt32 + 1, "-178956970 years -7 mons -2147483647 days -2562047788:00:54.775808"},
		{999999, 0, 0, "00:00:00.999999"},
	} {
		v, err := LookupOID(OIDInterval).DecodeBinary(intervalBin(c.us, c.day, c.mon))
		if err != nil || v != c.want {
			t.Errorf("us %d day %d mon %d: got %v err %v, want %q", c.us, c.day, c.mon, v, err, c.want)
		}
	}
	// PostgreSQL 17 and later: infinity is the maximum of all three fields.
	if v, _ := decodeIntervalBinary(intervalBin(math.MaxInt64, math.MaxInt32, math.MaxInt32)); v != "infinity" {
		t.Errorf("infinity: got %v", v)
	}
	if v, _ := decodeIntervalBinary(intervalBin(math.MinInt64, math.MinInt32, math.MinInt32)); v != "-infinity" {
		t.Errorf("-infinity: got %v", v)
	}
	for _, bad := range [][]byte{make([]byte, 15), make([]byte, 17), nil} {
		if v, err := decodeIntervalBinary(bad); err == nil {
			t.Errorf("% x: got %v, want an error", bad, v)
		}
	}
}
