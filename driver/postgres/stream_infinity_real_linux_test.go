//go:build linux

package postgres

import (
	"context"
	"database/sql/driver"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

// TestStreamedInfinityAgainstServer runs the streamed-row path on the real
// event loop against a real server (CELERIS_PG_DSN; skipped without it): a
// result of 3000 wide rows alternating 'infinity', '-infinity' and the real
// 9999-12-31 23:59:59, forced onto the asynchronous path so the rows are
// promoted to a streamRows after the 64th. Infinity into a *string is the
// server's text, a real value is itself (never "infinity"), *time.Time keeps the
// sentinels.
func TestStreamedInfinityAgainstServer(t *testing.T) {
	raw := os.Getenv("CELERIS_PG_DSN")
	if raw == "" {
		t.Skip("skipping: CELERIS_PG_DSN not set")
	}
	dsn, err := ParseDSN(raw)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	end := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	for _, c := range []struct {
		typ, real string
		realTime  time.Time
		realStr   string
	}{
		{"date", "9999-12-31", time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), "9999-12-31T00:00:00Z"},
		{"timestamp", "9999-12-31 23:59:59", end, "9999-12-31T23:59:59Z"},
		{"timestamptz", "9999-12-31 23:59:59+00", end, "9999-12-31T23:59:59Z"},
	} {
		q := `SELECT CASE i % 3 WHEN 0 THEN 'infinity'::` + c.typ + ` WHEN 1 THEN '-infinity'::` + c.typ +
			` ELSE '` + c.real + `'::` + c.typ + ` END, repeat('x', 1000) FROM generate_series(1, 3000) AS i`
		for _, mode := range []string{"simple", "extended"} {
			for _, asyncOnly := range []bool{true, false} {
				name := c.typ + "/" + mode
				if asyncOnly {
					name += "/async"
				} else {
					name += "/default"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					conn, err := dialConn(ctx, prov, nil, dsn, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					if asyncOnly {
						conn.syncLoop = nil // the asynchronous path, which streams
					}
					query, args := q, []driver.NamedValue(nil)
					if mode == "extended" {
						query += " WHERE $1::int = 1"
						args = []driver.NamedValue{{Ordinal: 1, Value: int64(1)}}
					}
					inner, err := conn.QueryContext(ctx, query, args)
					if err != nil {
						t.Fatal(err)
					}
					_, streamed := inner.(*streamRows)
					t.Logf("streamed=%v", streamed)
					if asyncOnly && !streamed {
						t.Fatalf("the asynchronous path did not stream: the test would prove nothing")
					}
					rows := acquireRows(inner, nil, nil)
					defer rows.Close()
					n := 0
					var s, pad string
					var tm time.Time
					for rows.Next() {
						n++
						if err := rows.Scan(&s, &pad); err != nil {
							t.Fatalf("row %d *string: %v", n, err)
						}
						if err := rows.Scan(&tm, &pad); err != nil {
							t.Fatalf("row %d *time.Time: %v", n, err)
						}
						var wantStr string
						var wantTime time.Time
						switch n % 3 {
						case 0:
							wantStr, wantTime = "infinity", PGInfinity
						case 1:
							wantStr, wantTime = "-infinity", PGNegInfinity
						default:
							wantTime = c.realTime
							switch {
							case streamed:
								wantStr = c.realTime.String() // a streamed row scans as in v1.5.11 (fmt's %v of the time.Time)
							case mode == "simple":
								wantStr = c.real // a buffered simple-protocol row hands over the server's text, as in v1.5.11
							default:
								wantStr = c.realStr // a buffered binary row: RFC3339Nano
							}
						}
						if s != wantStr {
							t.Fatalf("row %d: *string %q, want %q (streamed=%v)", n, s, wantStr, streamed)
						}
						if !tm.Equal(wantTime) {
							t.Fatalf("row %d: *time.Time %v, want %v", n, tm, wantTime)
						}
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					if n != 3000 {
						t.Fatalf("got %d rows", n)
					}
				})
			}
		}
	}
}
