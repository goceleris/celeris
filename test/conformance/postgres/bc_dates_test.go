//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestBCDates reads dates and timestamps before 1 AD through database/sql
// into time.Time, over the simple protocol (a query without arguments: text
// result columns) and, for dates, also over the extended protocol (a query
// with an argument: binary result columns), and requires the same value from
// both.
//
// The extended read of timestamp and timestamptz is not checked here: the
// binary timestamp decoder multiplies microseconds by time.Microsecond into a
// time.Duration, which overflows int64 outside 1708..2292, so every such value
// (BC or AD, e.g. year 3000) comes back wrong over the extended protocol. That
// is a separate bug that this change does not touch (it is listed in the PR
// description).
//
// PostgreSQL prints such a value as "YYYY-MM-DD BC" (and "... HH:MM:SS[.f][+tz] BC"
// for timestamps) with the year counted from 1 BC. Go's time package uses
// astronomical year numbering where 1 BC is year 0, so N BC is Go year 1-N:
// 0044-03-15 BC is year -43, 0001-01-01 BC is year 0. The proleptic Gregorian
// calendar is the same in both, so the day/second counts agree. Before the
// fix the simple protocol failed with `parsing time "0044-03-15 BC": extra
// text: " BC"`.
func TestBCDates(t *testing.T) {
	db := openDB(t)
	utc := time.UTC
	cases := []struct {
		name string
		typ  string
		in   string // PostgreSQL input, with the BC suffix
		want time.Time
	}{
		{"date_44BC", "date", "0044-03-15 BC", time.Date(-43, 3, 15, 0, 0, 0, 0, utc)},
		{"date_1BC_is_year_0", "date", "0001-01-01 BC", time.Date(0, 1, 1, 0, 0, 0, 0, utc)},
		{"date_leap_day_45BC", "date", "0045-02-29 BC", time.Date(-44, 2, 29, 0, 0, 0, 0, utc)},
		{"date_min_4713BC", "date", "4713-11-24 BC", time.Date(-4712, 11, 24, 0, 0, 0, 0, utc)},
		{"timestamp_44BC", "timestamp", "0044-03-15 12:34:56.789123 BC", time.Date(-43, 3, 15, 12, 34, 56, 789123000, utc)},
		{"timestamp_1BC", "timestamp", "0001-12-31 23:59:59 BC", time.Date(0, 12, 31, 23, 59, 59, 0, utc)},
		{"timestamptz_44BC", "timestamptz", "0044-03-15 12:34:56.789123+00 BC", time.Date(-43, 3, 15, 12, 34, 56, 789123000, utc)},
		{"timestamptz_offset", "timestamptz", "0044-03-15 12:34:56+05:30 BC", time.Date(-43, 3, 15, 7, 4, 56, 0, utc)},
		{"timestamptz_leap_day_45BC", "timestamptz", "0045-02-29 12:00:00+00 BC", time.Date(-44, 2, 29, 12, 0, 0, 0, utc)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET TIME ZONE 'UTC'"); err != nil {
		t.Fatalf("SET TIME ZONE: %v", err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			read := func(q string, args ...any) time.Time {
				t.Helper()
				var got time.Time
				if err := conn.QueryRowContext(ctx, q, args...).Scan(&got); err != nil {
					t.Fatalf("%s %v: %v", q, args, err)
				}
				return got
			}
			// No argument: simple protocol, text result column.
			simple := read("SELECT '" + c.in + "'::" + c.typ)
			if c.typ == "date" {
				// With an argument: extended protocol, binary result column.
				ext := read("SELECT $1::"+c.typ, c.in)
				if !ext.Equal(c.want) || ext.Year() != c.want.Year() {
					t.Errorf("extended: got %v (year %d), want %v (year %d)", ext, ext.Year(), c.want, c.want.Year())
				}
			}
			if !simple.Equal(c.want) || simple.Year() != c.want.Year() {
				t.Errorf("simple: got %v (year %d), want %v (year %d)", simple, simple.Year(), c.want, c.want.Year())
			}
		})
	}

	// Dates after 1 AD are unaffected, and a NULL BC-typed column is still NULL.
	t.Run("AD_and_NULL", func(t *testing.T) {
		var d time.Time
		if err := conn.QueryRowContext(ctx, "SELECT '0001-01-01'::date").Scan(&d); err != nil {
			t.Fatal(err)
		}
		if want := time.Date(1, 1, 1, 0, 0, 0, 0, utc); !d.Equal(want) {
			t.Fatalf("got %v, want %v", d, want)
		}
		var n sql.NullTime
		if err := conn.QueryRowContext(ctx, "SELECT NULL::date").Scan(&n); err != nil || n.Valid {
			t.Fatalf("NULL::date: %v valid=%v", err, n.Valid)
		}
	})
}
