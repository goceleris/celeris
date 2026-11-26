//go:build postgres

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestNativeInfinityVersusRealMaximum is the table for the sentinel dates:
// date, timestamp and timestamptz holding 'infinity', '-infinity' and the real
// value 9999-12-31 (23:59:59) that PGInfinity happens to equal, scanned through
// the simple, cached and unnamed paths into *string, *[]byte, *time.Time and
// *any. Each is compared with what the server itself prints (its ::text).
//
//   - infinity / -infinity into *string / *[]byte is the server's text
//     ("infinity" / "-infinity"), read from the wire value before any
//     time.Time exists. No in-band sentinel may report a real value as
//     something else.
//   - a real 9999-12-31 23:59:59 into *string / *[]byte is itself: RFC3339Nano
//     ("9999-12-31T23:59:59Z") on the binary paths (cached, unnamed), and the
//     server's own text ("9999-12-31 23:59:59") on the simple protocol, as in
//     v1.5.11. It is never "infinity".
//   - *time.Time and *any keep the documented sentinels (PGInfinity,
//     PGNegInfinity), as in v1.5.11: a real value that equals PGInfinity is
//     indistinguishable there, which the PGInfinity godoc says (check with
//     time.Equal, and use a string or a typed column where it matters).
func TestNativeInfinityVersusRealMaximum(t *testing.T) {
	dsn := dsnFromEnv(t)
	pools := map[string]*postgres.Pool{}
	for name, d := range map[string]string{"cached": dsn, "unnamed": withParams(dsn, "auto_cache_statements=false")} {
		pools[name] = openSettingsPool(t, d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	end := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	endDate := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, expr string
		wantStr    string    // *string / *[]byte
		wantTime   time.Time // *time.Time / *any
	}{
		{"date/infinity", "'infinity'::date", "infinity", postgres.PGInfinity},
		{"date/-infinity", "'-infinity'::date", "-infinity", postgres.PGNegInfinity},
		{"date/9999-12-31", "'9999-12-31'::date", "9999-12-31T00:00:00Z", endDate},
		{"timestamp/infinity", "'infinity'::timestamp", "infinity", postgres.PGInfinity},
		{"timestamp/-infinity", "'-infinity'::timestamp", "-infinity", postgres.PGNegInfinity},
		{"timestamp/9999-12-31", "'9999-12-31 23:59:59'::timestamp", "9999-12-31T23:59:59Z", end},
		{"timestamptz/infinity", "'infinity'::timestamptz", "infinity", postgres.PGInfinity},
		{"timestamptz/-infinity", "'-infinity'::timestamptz", "-infinity", postgres.PGNegInfinity},
		{"timestamptz/9999-12-31", "'9999-12-31 23:59:59+00'::timestamptz", "9999-12-31T23:59:59Z", end},
	} {
		// The server's own text for the infinities is the string a string
		// destination must hold.
		var serverText string
		if err := pools["cached"].QueryRow(ctx, "SELECT ("+c.expr+")::text").Scan(&serverText); err != nil {
			t.Fatalf("%s: server text: %v", c.name, err)
		}
		if c.wantStr == "infinity" || c.wantStr == "-infinity" {
			if serverText != c.wantStr {
				t.Fatalf("%s: server prints %q, table expects %q", c.name, serverText, c.wantStr)
			}
		} else {
			// A real value: the server's own text, parsed, is the same instant
			// as the table's expectation.
			got, ok := parseServerTime(serverText)
			if !ok || !got.Equal(c.wantTime) {
				t.Fatalf("%s: server prints %q (%v), table expects %v", c.name, serverText, got, c.wantTime)
			}
		}
		for _, mode := range []string{"simple", "cached", "unnamed"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				wantStr := c.wantStr
				p, q, args := pools["cached"], "SELECT "+c.expr, []any(nil)
				if mode != "simple" {
					q += " WHERE $1::int = 1"
					args = []any{1}
				}
				if mode == "unnamed" {
					p = pools["unnamed"]
				}
				if srv, ok := parseServerTime(serverText); ok && mode != "simple" {
					wantStr = srv.UTC().Format(time.RFC3339Nano) // the server's own value, as RFC3339Nano
				} else if mode == "simple" {
					wantStr = serverText // the simple protocol hands over the server's text, as in v1.5.11
				}
				var s string
				if err := p.QueryRow(ctx, q, args...).Scan(&s); err != nil || s != wantStr {
					t.Errorf("*string: got %q err %v, want %q (server text %q)", s, err, wantStr, serverText)
				}
				// A *[]byte is the value's wire bytes (binary paths) or the server's
				// text (simple protocol), as in v1.5.11; the text is in the *string.
				var b []byte
				if err := p.QueryRow(ctx, q, args...).Scan(&b); err != nil {
					t.Errorf("*[]byte: %v", err)
				} else if mode == "simple" && string(b) != serverText {
					t.Errorf("*[]byte (simple): got %q, want the server's text %q", b, serverText)
				} else if mode != "simple" && len(b) != 4 && len(b) != 8 {
					t.Errorf("*[]byte (%s): got %q, want the 4 or 8 wire bytes", mode, b)
				}
				var tm time.Time
				if err := p.QueryRow(ctx, q, args...).Scan(&tm); err != nil || !tm.Equal(c.wantTime) {
					t.Errorf("*time.Time: got %v err %v, want %v", tm, err, c.wantTime)
				}
				var a any
				if err := p.QueryRow(ctx, q, args...).Scan(&a); err != nil {
					t.Fatalf("*any: %v", err)
				}
				if at, ok := a.(time.Time); !ok || !at.Equal(c.wantTime) {
					t.Errorf("*any: got %#v, want time %v", a, c.wantTime)
				}
			})
		}
	}
}

// parseServerTime parses the server's ISO text of a date, timestamp or
// timestamptz (infinity excluded), as an instant in UTC.
func parseServerTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999Z07:00",
		"2006-01-02 15:04:05.999999", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
