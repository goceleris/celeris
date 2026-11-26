//go:build postgres

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestTimestampFullRange reads and writes date, timestamp and timestamptz
// across PostgreSQL's whole range through database/sql, over the simple
// protocol (text result columns) and the extended protocol (binary result
// columns and binary parameters), and requires the same value everywhere.
//
// The binary timestamp codec used to go through time.Duration, an int64 count
// of nanoseconds that spans only about 292 years either side of 2000-01-01:
// outside roughly 1708..2292 (AD or BC) a read over the extended protocol
// returned a silently wrong time (year 1500 came back as 2084, year 3000 as
// 1830) and a write sent a saturated, equally wrong value.
func TestTimestampFullRange(t *testing.T) {
	db := openDB(t)
	utc := time.UTC
	cases := []struct {
		name string
		lit  string // PostgreSQL input literal
		want time.Time
	}{
		{"year_1", "0001-01-01 00:00:00", time.Date(1, 1, 1, 0, 0, 0, 0, utc)},
		{"year_1500", "1500-01-01 00:00:00", time.Date(1500, 1, 1, 0, 0, 0, 0, utc)},
		{"year_1707", "1707-01-01 00:00:00", time.Date(1707, 1, 1, 0, 0, 0, 0, utc)},
		{"year_1708", "1708-01-01 12:34:56.789123", time.Date(1708, 1, 1, 12, 34, 56, 789123000, utc)},
		{"year_2292", "2292-12-31 23:59:59.999999", time.Date(2292, 12, 31, 23, 59, 59, 999999000, utc)},
		{"year_2293", "2293-01-01 00:00:00", time.Date(2293, 1, 1, 0, 0, 0, 0, utc)},
		{"year_3000", "3000-01-01 00:00:00", time.Date(3000, 1, 1, 0, 0, 0, 0, utc)},
		{"year_9999", "9999-12-31 23:59:59.999999", time.Date(9999, 12, 31, 23, 59, 59, 999999000, utc)},
		{"current", "2026-11-22 10:11:12.131415", time.Date(2026, 11, 22, 10, 11, 12, 131415000, utc)},
		{"bc_44", "0044-03-15 12:34:56.789123 BC", time.Date(-43, 3, 15, 12, 34, 56, 789123000, utc)},
		{"bc_1", "0001-12-31 23:59:59 BC", time.Date(0, 12, 31, 23, 59, 59, 0, utc)},
		{"bc_4713", "4713-01-01 00:00:00 BC", time.Date(-4712, 1, 1, 0, 0, 0, 0, utc)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET TIME ZONE 'UTC'"); err != nil {
		t.Fatalf("SET TIME ZONE: %v", err)
	}
	oids := map[string]uint32{"timestamp": 1114, "timestamptz": 1184, "date": 1082}
	for _, typ := range []string{"timestamp", "timestamptz", "date"} {
		for _, c := range cases {
			want := c.want
			lit := c.lit
			if typ == "date" {
				want = time.Date(want.Year(), want.Month(), want.Day(), 0, 0, 0, 0, utc)
				lit = lit[:findSpace(lit)]
				if c.want.Year() <= 0 {
					lit += " BC"
				}
			}
			pgcodec := postgres.LookupOID(oids[typ])
			t.Run(typ+"/"+c.name, func(t *testing.T) {
				readTime := func(q string, args ...any) time.Time {
					t.Helper()
					var got time.Time
					if err := conn.QueryRowContext(ctx, q, args...).Scan(&got); err != nil {
						t.Fatalf("%s %v: %v", q, args, err)
					}
					return got
				}
				same := func(what string, got time.Time) {
					t.Helper()
					if !got.Equal(want) || got.Year() != want.Year() {
						t.Errorf("%s: got %v (year %d), want %v (year %d)", what, got, got.Year(), want, want.Year())
					}
				}
				// No argument: simple protocol, text result column.
				same("simple read", readTime("SELECT '"+lit+"'::"+typ))
				// With an argument: extended protocol, binary result column.
				same("extended read", readTime("SELECT $1::"+typ, lit))
				// Binary parameter: the codec's own EncodeBinary output, sent as
				// a []byte (binary format) to a $1 the server types as typ, must
				// reach the server unchanged. Compare what the server prints for
				// the parameter with what it prints for the literal.
				var wantText, gotText string
				if err := conn.QueryRowContext(ctx, "SELECT '"+lit+"'::"+typ+"::text").Scan(&wantText); err != nil {
					t.Fatalf("server text: %v", err)
				}
				wire, err := pgcodec.EncodeBinary(nil, want)
				if err != nil {
					t.Fatalf("EncodeBinary(%v): %v", want, err)
				}
				if err := conn.QueryRowContext(ctx, "SELECT $1::"+typ+"::text", wire).Scan(&gotText); err != nil {
					t.Fatalf("binary write: %v", err)
				}
				if gotText != wantText {
					t.Errorf("binary write: server saw %q, want %q", gotText, wantText)
				}
				// A time.Time argument goes as RFC 3339 text (the driver's scalar
				// path); the value must survive that and the binary read.
				if want.Year() >= 1 {
					same("time.Time round trip", readTime("SELECT $1::"+typ, want))
				}
			})
		}
	}
}

func findSpace(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return i
		}
	}
	return len(s)
}
