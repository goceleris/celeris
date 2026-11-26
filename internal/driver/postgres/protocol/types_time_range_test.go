package protocol

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// time.Duration counts nanoseconds in an int64, so it spans only about
// +-292 years. The binary timestamp/timestamptz decoder used to compute
// pgEpoch.Add(time.Duration(micros) * time.Microsecond), which overflows
// int64 for every value outside roughly years 1708..2292 and returned a
// silently wrong time (year 1500 read back as 2084, year 3000 as 1830); the
// encoders computed t.Sub(pgEpoch), which saturates at the same bounds and
// wrote a silently wrong value. PostgreSQL's range (4714 BC to 294276 AD)
// needs the full int64 of microseconds, so the codecs now split the value
// into seconds and a microsecond remainder and use time.Unix.

// rangeCases are server-derived (postgres:18.7):
//
//	SELECT (extract(epoch FROM v::timestamp) - 946684800) * 1000000,
//	       v::date - '2000-01-01'::date
//
// micros is the wire value (microseconds since 2000-01-01 00:00:00), days the
// date wire value, valid only when the time of day is midnight.
var rangeCases = []struct {
	name   string
	t      time.Time // Go value (astronomical years: 1 BC is year 0)
	micros int64
	days   int32 // valid only when hasDay (t is at midnight)
	hasDay bool
}{
	{"year_1", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), -63082281600000000, -730119, true},
	{"year_1500", time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), -15778454400000000, -182621, true},
	{"year_1707", time.Date(1707, 1, 1, 0, 0, 0, 0, time.UTC), -9246182400000000, -107016, true},
	{"year_1708_frac", time.Date(1708, 1, 1, 12, 34, 56, 789123000, time.UTC), -9214601103210877, 0, false},
	{"year_2292_end", time.Date(2292, 12, 31, 23, 59, 59, 999999000, time.UTC), 9246268799999999, 0, false},
	{"year_2293", time.Date(2293, 1, 1, 0, 0, 0, 0, time.UTC), 9246268800000000, 107017, true},
	{"year_3000", time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), 31556995200000000, 365243, true},
	{"year_9999_end", time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), 252455615999999999, 0, false},
	{"bc_44_frac", time.Date(-43, 3, 15, 12, 34, 56, 789123000, time.UTC), -64464463503210877, 0, false},
	{"bc_4713_jan", time.Date(-4712, 1, 1, 0, 0, 0, 0, time.UTC), -211810204800000000, -2451507, true},
	{"bc_4713_nov24", time.Date(-4712, 11, 24, 0, 0, 0, 0, time.UTC), -211781865600000000, -2451179, true},
	{"current_era", time.Date(2026, 11, 22, 10, 11, 12, 131415000, time.UTC), 848657472131415, 0, false},
	{"current_era_midnight", time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC), 848620800000000, 9822, true},
}

func putU64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func TestTimestampBinaryFullRange(t *testing.T) {
	for _, oid := range []uint32{OIDTimestamp, OIDTimestamptz} {
		c := LookupOID(oid)
		for _, tc := range rangeCases {
			want := tc.t
			micros := tc.micros
			t.Run(c.Name+"/"+tc.name, func(t *testing.T) {
				got, err := c.DecodeBinary(putU64(micros))
				if err != nil {
					t.Fatal(err)
				}
				if g := got.(time.Time); !g.Equal(want) || g.Year() != want.Year() {
					t.Errorf("decode(%d) = %v, want %v", micros, g, want)
				}
				buf, err := c.EncodeBinary(nil, want)
				if err != nil {
					t.Fatal(err)
				}
				if g := int64(binary.BigEndian.Uint64(buf)); g != micros {
					t.Errorf("encode(%v) = %d, want %d", want, g, micros)
				}
			})
		}
	}
}

func TestDateBinaryFullRange(t *testing.T) {
	c := LookupOID(OIDDate)
	for _, tc := range rangeCases {
		if !tc.hasDay {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(tc.days))
			got, err := c.DecodeBinary(b[:])
			if err != nil {
				t.Fatal(err)
			}
			if g := got.(time.Time); !g.Equal(tc.t) || g.Year() != tc.t.Year() {
				t.Errorf("decode(%d) = %v, want %v", tc.days, g, tc.t)
			}
			buf, err := c.EncodeBinary(nil, tc.t)
			if err != nil {
				t.Fatal(err)
			}
			if g := int32(binary.BigEndian.Uint32(buf)); g != tc.days {
				t.Errorf("encode(%v) = %d, want %d", tc.t, g, tc.days)
			}
		})
	}
}

// A non-UTC time.Time is encoded by its instant, and a date by its UTC day.
func TestTimestampBinaryEncodeNonUTCFarRange(t *testing.T) {
	loc := time.FixedZone("x", 5*3600+30*60)
	tt := time.Date(3000, 1, 1, 5, 30, 0, 0, loc) // 3000-01-01 00:00:00 UTC
	buf, err := LookupOID(OIDTimestamptz).EncodeBinary(nil, tt)
	if err != nil {
		t.Fatal(err)
	}
	if g := int64(binary.BigEndian.Uint64(buf)); g != 31556995200000000 {
		t.Errorf("got %d", g)
	}
}

// The infinity sentinels still decode to PGInfinity / PGNegInfinity.
func TestTimeBinaryInfinity(t *testing.T) {
	for _, oid := range []uint32{OIDTimestamp, OIDTimestamptz} {
		c := LookupOID(oid)
		if v, _ := c.DecodeBinary(putU64(math.MaxInt64)); !v.(time.Time).Equal(PGInfinity) {
			t.Errorf("%s +infinity = %v", c.Name, v)
		}
		if v, _ := c.DecodeBinary(putU64(math.MinInt64)); !v.(time.Time).Equal(PGNegInfinity) {
			t.Errorf("%s -infinity = %v", c.Name, v)
		}
	}
	c := LookupOID(OIDDate)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.MaxInt32)
	if v, _ := c.DecodeBinary(b[:]); !v.(time.Time).Equal(PGInfinity) {
		t.Errorf("date +infinity = %v", v)
	}
	binary.BigEndian.PutUint32(b[:], 1<<31)
	if v, _ := c.DecodeBinary(b[:]); !v.(time.Time).Equal(PGNegInfinity) {
		t.Errorf("date -infinity = %v", v)
	}
}

// A time.Time whose microseconds since 2000-01-01 do not fit in int64 (or
// whose day count does not fit in int32) is an error, never a wrapped or
// saturated wire value.
func TestTimeBinaryEncodeOutOfRange(t *testing.T) {
	far := time.Date(400000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, oid := range []uint32{OIDTimestamp, OIDTimestamptz} {
		if _, err := LookupOID(oid).EncodeBinary(nil, far); err == nil {
			t.Errorf("oid %d: year 400000 encoded without error", oid)
		}
		if _, err := LookupOID(oid).EncodeBinary(nil, far.AddDate(-800000, 0, 0)); err == nil {
			t.Errorf("oid %d: year -400000 encoded without error", oid)
		}
	}
	if _, err := LookupOID(OIDDate).EncodeBinary(nil, time.Date(6_000_000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Error("date: year 6000000 encoded without error")
	}
}
