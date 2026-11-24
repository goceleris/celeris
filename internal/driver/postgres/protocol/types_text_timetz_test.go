package protocol

import (
	"encoding/binary"
	"testing"
)

func timetzBin(us int64, zoneWest int32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b, uint64(us))
	binary.BigEndian.PutUint32(b[8:], uint32(zoneWest))
	return b
}

func TestDecodeTimetzBinary(t *testing.T) {
	const noon = 12 * usecsPerHour
	for _, c := range []struct {
		us   int64
		zone int32 // seconds west of UTC, as stored
		want string
	}{
		{0, 0, "00:00:00+00"},
		{noon, 0, "12:00:00+00"},
		{noon, -5 * 3600, "12:00:00+05"},
		{noon, 5 * 3600, "12:00:00-05"},
		{noon, -(5*3600 + 30*60), "12:00:00+05:30"},
		{noon, 5*3600 + 30*60, "12:00:00-05:30"},
		{noon, -(15*3600 + 59*60 + 59), "12:00:00+15:59:59"},
		{noon, 15*3600 + 59*60 + 59, "12:00:00-15:59:59"},
		{noon, -1, "12:00:00+00:00:01"},
		{noon, 1, "12:00:00-00:00:01"},
		{noon, -3601, "12:00:00+01:00:01"},
		{usecsPerDay, 0, "24:00:00+00"},
		{86399999999, -(5*3600 + 30*60), "23:59:59.999999+05:30"},
	} {
		v, err := LookupOID(OIDTimetz).DecodeBinary(timetzBin(c.us, c.zone))
		if err != nil || v != c.want {
			t.Errorf("%d zone %d: got %v err %v, want %q", c.us, c.zone, v, err, c.want)
		}
	}
	for _, bad := range [][]byte{timetzBin(-1, 0), timetzBin(usecsPerDay+1, 0), timetzBin(0, 16*3600), timetzBin(0, -16*3600), make([]byte, 8), nil} {
		if v, err := decodeTimetzBinary(bad); err == nil {
			t.Errorf("% x: got %v, want an error", bad, v)
		}
	}
}
