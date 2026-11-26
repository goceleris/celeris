//go:build postgres

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestNativeScanPathParity reads one value of each common type through the
// native Pool on the two extended paths and requires an identical result from
// each:
//
//   - cached: the DSN default (auto-cached prepared statement, binary results);
//   - unnamed: auto_cache_statements=false (unnamed statement, binary results).
//
// What a *string, *[]byte, *any or *float64 destination receives must not
// depend on auto_cache_statements. The simple protocol (no arguments) reads
// text and keeps its v1.5.11 results: see TestNativeSimplePathKeepsServerText.
func TestNativeScanPathParity(t *testing.T) {
	// The default session time zone, and one with a non-zero offset (a
	// timestamptz prints with it on the text paths).
	t.Run("tz-default", func(t *testing.T) { testNativeScanPathParity(t, dsnFromEnv(t)) })
	// Before 1937 Amsterdam is at +00:19:32: the offset has seconds.
	t.Run("tz-amsterdam", func(t *testing.T) {
		testNativeScanPathParity(t, withParams(dsnFromEnv(t), "TimeZone=Europe/Amsterdam"))
	})
	t.Run("tz-kolkata", func(t *testing.T) { testNativeScanPathParity(t, withParams(dsnFromEnv(t), "TimeZone=Asia/Kolkata")) })
}

func testNativeScanPathParity(t *testing.T, dsn string) {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	open := func(d string) *postgres.Pool {
		p, err := postgres.Open(d, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	cached := open(dsn)
	unnamed := open(dsn + sep + "auto_cache_statements=false")

	cases := []struct {
		name string
		expr string
		f64  bool // also scan into *float64
	}{
		{"bool_true", `true`, false},
		{"bool_false", `false`, false},
		{"date", `'2024-01-02'::date`, false},
		{"date_inf", `'infinity'::date`, false},
		{"timestamp", `'2024-01-02 03:04:05.123456'::timestamp`, false},
		{"timestamp_neginf", `'-infinity'::timestamp`, false},
		{"timestamptz", `'2024-01-02 03:04:05.123456+00'::timestamptz`, false},
		{"timestamptz_micro", `'2024-06-30 23:59:59.999999+05:30'::timestamptz`, false},
		{"timestamptz_1900", `'1900-01-01 12:00:00.5+00'::timestamptz`, false},
		{"timestamptz_inf", `'infinity'::timestamptz`, false},
		{"float4", `1.1::float4`, true},
		{"float4_big", `16777216::float4`, true},
		{"float8", `123456789012.5::float8`, true},
		{"float8_third", `0.30000000000000004::float8`, true},
		{"float8_small", `1e-7::float8`, true},
		{"float8_nan", `'NaN'::float8`, true},
		{"float8_inf", `'-Infinity'::float8`, true},
		{"int2", `-7::int2`, true},
		{"int4", `123456::int4`, true},
		{"int8", `9223372036854775807::int8`, true},
		{"text", `'héllo'::text`, false},
		{"varchar", `'abc'::varchar(8)`, false},
		{"numeric", `'1.50'::numeric`, false},
		{"numeric_neginf", `'-Infinity'::numeric`, false},
		{"uuid", `'123e4567-e89b-12d3-a456-426614174000'::uuid`, false},
		{"json", `'{"a": 1}'::json`, false},
		{"jsonb", `'{"a": 1}'::jsonb`, false},
		{"bytea", `'abc'::bytea`, false},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	render := func(v any, err error) string {
		if err != nil {
			return "ERR"
		}
		return fmt.Sprintf("%T %v", v, v)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			type scan func(p *postgres.Pool, q string, args ...any) string
			dests := map[string]scan{
				"*string": func(p *postgres.Pool, q string, args ...any) string {
					var s string
					err := p.QueryRow(ctx, q, args...).Scan(&s)
					return render(s, err)
				},
				"*[]byte": func(p *postgres.Pool, q string, args ...any) string {
					var b []byte
					err := p.QueryRow(ctx, q, args...).Scan(&b)
					return render(string(b), err)
				},
				"*any": func(p *postgres.Pool, q string, args ...any) string {
					var a any
					err := p.QueryRow(ctx, q, args...).Scan(&a)
					if b, ok := a.([]byte); ok {
						a = string(b) + "(bytes)"
					}
					return render(a, err)
				},
			}
			if c.f64 {
				dests["*float64"] = func(p *postgres.Pool, q string, args ...any) string {
					var f float64
					err := p.QueryRow(ctx, q, args...).Scan(&f)
					return render(f, err)
				}
			}
			for dn, sc := range dests {
				cach := sc(cached, "SELECT "+c.expr+" WHERE $1::int = 1", 1)
				unn := sc(unnamed, "SELECT "+c.expr+" WHERE $1::int = 1", 1)
				if cach != unn {
					t.Errorf("%s: paths differ:\n  cached  %s\n  unnamed %s", dn, cach, unn)
				}
			}
		})
	}
}

// TestNativeSimplePathKeepsServerText: the simple protocol reads text and the
// driver hands a string destination the server's own text, as v1.5.11 did (the
// cached and unnamed paths read binary and format the decoded value). Only
// bytea differs from the server's text: the text form is the hex encoding, and
// the bytes are the value.
func TestNativeSimplePathKeepsServerText(t *testing.T) {
	pool := openSettingsPool(t, dsnFromEnv(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, c := range []struct{ expr, want string }{
		{`true`, "t"},
		{`false`, "f"},
		{`'2024-01-02'::date`, "2024-01-02"},
		{`'2024-01-02 03:04:05.5'::timestamp`, "2024-01-02 03:04:05.5"},
		{`1.1::float4`, "1.1"},
		{`123456789012.5::float8`, "123456789012.5"},
		{`'Infinity'::float8`, "Infinity"},
		{`'12:34:56'::time`, "12:34:56"},
		{`'1 day 02:03:04'::interval`, "1 day 02:03:04"},
		{`'10.0.0.1'::inet`, "10.0.0.1"},
		{`ARRAY[1,2]::int4[]`, "{1,2}"},
	} {
		var s string
		if err := pool.QueryRow(ctx, "SELECT "+c.expr).Scan(&s); err != nil || s != c.want {
			t.Errorf("%s: got %q err %v, want the server's text %q", c.expr, s, err, c.want)
		}
	}
	var f float64
	if err := pool.QueryRow(ctx, "SELECT 1.1::float4").Scan(&f); err != nil || f != 1.1 {
		t.Errorf("float4 1.1 into *float64 on the simple path: got %v err %v, want 1.1 (v1.5.11)", f, err)
	}
	var b []byte
	if err := pool.QueryRow(ctx, `SELECT 'abc'::bytea`).Scan(&b); err != nil || string(b) != "abc" {
		t.Errorf("bytea on the simple path: got %q err %v, want the bytes", b, err)
	}
}
