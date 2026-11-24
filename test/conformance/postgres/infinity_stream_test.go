//go:build postgres

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestNativeInfinityStreamed reads a result of 150 rows (more than the 64 at
// which the event loop switches to streaming) whose rows alternate between
// 'infinity', '-infinity' and the real 9999-12-31 23:59:59 for date, timestamp
// and timestamptz, through the simple, cached and unnamed paths. A streamed row
// must scan like a buffered one: infinity into *string / *[]byte is the
// server's text, the real value is itself, *time.Time and *any keep the
// documented sentinels.
func TestNativeInfinityStreamed(t *testing.T) {
	dsn := dsnFromEnv(t)
	pools := map[string]*postgres.Pool{
		"cached":  openSettingsPool(t, dsn),
		"unnamed": openSettingsPool(t, withParams(dsn, "auto_cache_statements=false")),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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
			` ELSE '` + c.real + `'::` + c.typ + ` END FROM generate_series(1, 150) AS i`
		for _, mode := range []string{"simple", "cached", "unnamed"} {
			t.Run(c.typ+"/"+mode, func(t *testing.T) {
				p, query, args := pools["cached"], q, []any(nil)
				if mode != "simple" {
					query += " WHERE $1::int = 1"
					args = []any{1}
				}
				if mode == "unnamed" {
					p = pools["unnamed"]
				}
				rows, err := p.QueryContext(ctx, query, args...)
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				defer rows.Close()
				n := 0
				for rows.Next() {
					n++
					var s string
					var b []byte
					var tm time.Time
					var a any
					// one scan per destination kind: Scan decodes from the same row
					if err := rows.Scan(&s); err != nil {
						t.Fatalf("row %d *string: %v", n, err)
					}
					if err := rows.Scan(&b); err != nil {
						t.Fatalf("row %d *[]byte: %v", n, err)
					}
					if err := rows.Scan(&tm); err != nil {
						t.Fatalf("row %d *time.Time: %v", n, err)
					}
					if err := rows.Scan(&a); err != nil {
						t.Fatalf("row %d *any: %v", n, err)
					}
					var wantStr string
					var wantTime time.Time
					switch n % 3 { // i = n
					case 0:
						wantStr, wantTime = "infinity", postgres.PGInfinity
					case 1:
						wantStr, wantTime = "-infinity", postgres.PGNegInfinity
					default:
						wantStr, wantTime = c.realStr, c.realTime
					}
					if s != wantStr || string(b) != wantStr {
						t.Errorf("row %d: *string %q, *[]byte %q, want %q", n, s, b, wantStr)
					}
					if !tm.Equal(wantTime) {
						t.Errorf("row %d: *time.Time %v, want %v", n, tm, wantTime)
					}
					if at, ok := a.(time.Time); !ok || !at.Equal(wantTime) {
						t.Errorf("row %d: *any %#v, want %v", n, a, wantTime)
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if n != 150 {
					t.Fatalf("got %d rows, want 150", n)
				}
			})
		}
	}
}
