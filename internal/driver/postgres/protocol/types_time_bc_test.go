package protocol

import (
	"encoding/binary"
	"testing"
	"time"
)

// PostgreSQL prints a date or timestamp before 1 AD with a trailing " BC" and
// a positive year counted from 1 BC: "0044-03-15 BC" is 44 BC. Go's time
// package uses astronomical year numbering, in which 1 BC is year 0, 2 BC is
// year -1 and N BC is year 1-N, so "0044-03-15 BC" is year -43. The binary
// decoders (days or microseconds from 2000-01-01 over the proleptic Gregorian
// calendar, which both PostgreSQL and Go use) already return that mapping;
// the text decoders used by the simple protocol must return the same value
// instead of failing with `extra text: " BC"`.

// bcDayCounts are server-derived: SELECT '<date>'::date - '2000-01-01'::date
// on PostgreSQL 14, 16 and 18.
var bcDayCounts = []struct {
	text string
	days int32
}{
	{"0044-03-15 BC", -746117},
	{"0001-01-01 BC", -730485},
	{"0001-12-31 BC", -730120},
	{"0045-02-29 BC", -746497}, // 45 BC is a leap year (astronomical -44)
	{"4713-11-24 BC", -2451179},
}

func TestDateTextBCMatchesBinary(t *testing.T) {
	c := LookupOID(OIDDate)
	for _, tc := range bcDayCounts {
		t.Run(tc.text, func(t *testing.T) {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(tc.days))
			want, err := c.DecodeBinary(b[:])
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.DecodeText([]byte(tc.text))
			if err != nil {
				t.Fatalf("DecodeText(%q): %v", tc.text, err)
			}
			if !got.(time.Time).Equal(want.(time.Time)) {
				t.Fatalf("DecodeText(%q) = %v, binary says %v", tc.text, got, want)
			}
		})
	}
}

func TestTimeTextBC(t *testing.T) {
	utc := time.UTC
	tests := []struct {
		name string
		oid  uint32
		text string
		want time.Time
	}{
		// date: N BC is Go year 1-N.
		{"date_44BC", OIDDate, "0044-03-15 BC", time.Date(-43, 3, 15, 0, 0, 0, 0, utc)},
		{"date_1BC_is_year_0", OIDDate, "0001-01-01 BC", time.Date(0, 1, 1, 0, 0, 0, 0, utc)},
		{"date_1BC_end", OIDDate, "0001-12-31 BC", time.Date(0, 12, 31, 0, 0, 0, 0, utc)},
		{"date_leap_day_45BC", OIDDate, "0045-02-29 BC", time.Date(-44, 2, 29, 0, 0, 0, 0, utc)},
		{"date_min", OIDDate, "4713-11-24 BC", time.Date(-4712, 11, 24, 0, 0, 0, 0, utc)},
		{"date_AD_unchanged", OIDDate, "0001-01-01", time.Date(1, 1, 1, 0, 0, 0, 0, utc)},

		// timestamp
		{"timestamp_44BC", OIDTimestamp, "0044-03-15 12:34:56 BC", time.Date(-43, 3, 15, 12, 34, 56, 0, utc)},
		{"timestamp_frac", OIDTimestamp, "0044-03-15 12:34:56.789123 BC", time.Date(-43, 3, 15, 12, 34, 56, 789123000, utc)},
		{"timestamp_leap_day", OIDTimestamp, "0045-02-29 23:59:59 BC", time.Date(-44, 2, 29, 23, 59, 59, 0, utc)},
		{"timestamp_1BC", OIDTimestamp, "0001-01-01 00:00:00 BC", time.Date(0, 1, 1, 0, 0, 0, 0, utc)},

		// timestamptz (the server prints it in the session zone; UTC here)
		{"timestamptz_44BC", OIDTimestamptz, "0044-03-15 12:34:56+00 BC", time.Date(-43, 3, 15, 12, 34, 56, 0, utc)},
		{"timestamptz_frac", OIDTimestamptz, "0044-03-15 12:34:56.789123+00 BC", time.Date(-43, 3, 15, 12, 34, 56, 789123000, utc)},
		{"timestamptz_offset", OIDTimestamptz, "0044-03-15 12:34:56+05:30 BC", time.Date(-43, 3, 15, 7, 4, 56, 0, utc)},
		{"timestamptz_leap_day", OIDTimestamptz, "0045-02-29 12:00:00+00 BC", time.Date(-44, 2, 29, 12, 0, 0, 0, utc)},
		{"timestamptz_offset_across_year", OIDTimestamptz, "0001-01-01 00:00:00+05:30 BC", time.Date(-1, 12, 31, 18, 30, 0, 0, utc)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LookupOID(tc.oid).DecodeText([]byte(tc.text))
			if err != nil {
				t.Fatalf("DecodeText(%q): %v", tc.text, err)
			}
			gt := got.(time.Time)
			if !gt.Equal(tc.want) || gt.Year() != tc.want.Year() {
				t.Fatalf("DecodeText(%q) = %v (year %d), want %v (year %d)", tc.text, gt, gt.Year(), tc.want, tc.want.Year())
			}
		})
	}
}

// A malformed BC value stays an error (never a silently wrong time).
func TestTimeTextBCMalformed(t *testing.T) {
	for _, tc := range []struct {
		oid  uint32
		text string
	}{
		{OIDDate, "0044-02-29 BC"}, // 44 BC is not a leap year
		{OIDDate, "0000-01-01 BC"}, // there is no year 0 in PostgreSQL's output
		{OIDDate, "0044-13-01 BC"},
		{OIDDate, "0044-03-15 BCE"},
		{OIDDate, " BC"},
		{OIDTimestamp, "0044-03-15 25:00:00 BC"},
		{OIDTimestamptz, "0044-03-15 12:00:00 BC"}, // timestamptz always carries an offset
	} {
		if v, err := LookupOID(tc.oid).DecodeText([]byte(tc.text)); err == nil {
			t.Errorf("DecodeText(%q) = %v, want an error", tc.text, v)
		}
	}
}
