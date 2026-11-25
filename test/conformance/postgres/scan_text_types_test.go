//go:build postgres

package postgres_test

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestNativeScanVersionedTextTypes covers the native Pool.QueryContext /
// Pool.QueryRow Scan fast path against a real server, in both protocols:
//
//   - simple: a statement with no arguments, results come back as text;
//   - extended: a statement with arguments, results are requested in binary.
//
// A jsonb or jsonpath cell scanned into *string, *json.RawMessage or *any must
// be the value's text, never the binary wire form (a *[]byte is the wire bytes,
// as in v1.5.11) (a 0x01 version
// byte followed by the text). The same column read through database/sql is
// the reference; lib/pq and pgx return the same text.
func TestNativeScanVersionedTextTypes(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cols := []struct {
		name string
		expr string
		want string
		json bool // valid JSON text: also scan into json.RawMessage
		ext  bool // extended protocol only (the simple protocol hands over the server's text form)
	}{
		{"jsonb", `'["popular", "new"]'::jsonb`, `["popular", "new"]`, true, false},
		{"json", `'{"a": 1}'::json`, `{"a": 1}`, true, false},
		{"jsonpath", `'$.a'::jsonpath`, `$."a"`, false, false},
		{"text", `'plain'::text`, "plain", false, false},
		{"bytea", `'abc'::bytea`, "abc", false, false},
		{"int4", `123::int4`, "123", false, false},
		{"date", `'2026-01-02'::date`, "2026-01-02T00:00:00Z", false, true},
		{"timestamp", `'2026-01-02 03:04:05'::timestamp`, "2026-01-02T03:04:05Z", false, true},
		{"timestamptz", `'2026-01-02 03:04:05+00'::timestamptz`, "2026-01-02T03:04:05Z", false, true},
		{"numeric-inf", `'-Infinity'::numeric`, "-Infinity", false, false},
	}
	for _, mode := range []string{"simple", "extended"} {
		for _, c := range cols {
			if c.ext && mode == "simple" {
				continue
			}
			q := "SELECT " + c.expr
			var args []any
			if mode == "extended" {
				q += " WHERE $1::int = 1"
				args = []any{1}
			}
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				// A native *[]byte is the value's wire bytes (extended protocol, binary
				// results) or the server's text (simple protocol), exactly as in v1.5.11;
				// scan into a string for text.
				var b []byte
				wantB := c.want
				if mode == "extended" {
					switch c.name {
					case "jsonb", "jsonpath":
						wantB = "\x01" + c.want
					case "int4":
						wantB = "\x00\x00\x00{"
					case "date", "timestamp", "timestamptz", "numeric-inf":
						wantB = "" // wire bytes of a fixed layout: only that it is not the text
					}
				}
				if err := pool.QueryRow(ctx, q, args...).Scan(&b); err != nil {
					t.Errorf("native *[]byte: %v", err)
				} else if wantB != "" && string(b) != wantB {
					t.Errorf("native *[]byte: got %q, want %q", b, wantB)
				} else if wantB == "" && (len(b) == 0 || string(b) == c.want) {
					t.Errorf("native *[]byte: got %q, want the wire bytes, not the text %q", b, c.want)
				}
				var s string
				if err := pool.QueryRow(ctx, q, args...).Scan(&s); err != nil || s != c.want {
					t.Errorf("native *string: got %q err %v, want %q", s, err, c.want)
				}
				if c.json {
					var rm json.RawMessage
					if err := pool.QueryRow(ctx, q, args...).Scan(&rm); err != nil || string(rm) != c.want {
						t.Errorf("native *json.RawMessage: got %q err %v, want %q", rm, err, c.want)
					}
				}
				var a any
				if err := pool.QueryRow(ctx, q, args...).Scan(&a); err != nil {
					t.Errorf("native *any: %v", err)
				} else if got := anyText(a); c.name != "int4" && !c.ext && got != c.want {
					t.Errorf("native *any: got %#v, want %q", a, c.want)
				}
				// database/sql reference.
				var ds string
				if err := db.QueryRowContext(ctx, q, args...).Scan(&ds); err != nil || ds != c.want {
					t.Errorf("database/sql *string: got %q err %v, want %q", ds, err, c.want)
				}
				var db2 []byte
				if err := db.QueryRowContext(ctx, q, args...).Scan(&db2); err != nil || string(db2) != c.want {
					t.Errorf("database/sql *[]byte: got %q err %v, want %q", db2, err, c.want)
				}
			})
		}
	}
}

// TestNativeScanJSONArrays checks that arrays of jsonb / json / jsonpath
// never surface a version byte. The simple protocol returns the array's text
// form; the extended protocol decodes the binary array into []driver.Value
// whose elements are the element texts (as for the other array codecs).
func TestNativeScanJSONArrays(t *testing.T) {
	db := openDB(t)
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, c := range []struct{ name, expr, elem string }{
		{"jsonb[]", `ARRAY['{"a": 1}'::jsonb]`, `{"a": 1}`},
		{"json[]", `ARRAY['{"a": 1}'::json]`, `{"a": 1}`},
		{"jsonpath[]", `ARRAY['$.a'::jsonpath]`, `$."a"`},
	} {
		t.Run("extended/"+c.name, func(t *testing.T) {
			q := "SELECT " + c.expr + " WHERE $1::int = 1"
			var a any
			if err := pool.QueryRow(ctx, q, 1).Scan(&a); err != nil {
				t.Fatalf("native *any: %v", err)
			}
			vals, ok := a.([]driver.Value)
			if !ok || len(vals) != 1 || anyText(vals[0]) != c.elem {
				t.Errorf("native *any: got %#v, want [%q]", a, c.elem)
			}
			var da any
			if err := db.QueryRowContext(ctx, q, 1).Scan(&da); err == nil {
				if vals, ok := da.([]driver.Value); !ok || len(vals) != 1 || anyText(vals[0]) != c.elem {
					t.Errorf("database/sql *any: got %#v, want [%q]", da, c.elem)
				}
			}
		})
		t.Run("simple/"+c.name, func(t *testing.T) {
			var s string
			if err := pool.QueryRow(ctx, "SELECT "+c.expr).Scan(&s); err != nil {
				t.Fatalf("native *string: %v", err)
			}
			if len(s) == 0 || s[0] != '{' || s[len(s)-1] != '}' {
				t.Errorf("native *string: got %q, want a text array", s)
			}
		})
	}
}

func anyText(a any) string {
	switch v := a.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	}
	return ""
}

// TestNativeScanInfinityIntoString checks that a date, timestamp or timestamptz
// 'infinity' / '-infinity' scanned into a *string reads back as the
// server's own text on every path: simple (no arguments), cached (the DSN
// default) and unnamed (auto_cache_statements=false). The decoded time.Time
// is a sentinel (PGInfinity, PGNegInfinity); formatting it as RFC3339Nano
// leaked "9999-12-31T23:59:59Z" on the binary paths.
func TestNativeScanInfinityIntoString(t *testing.T) {
	dsn := dsnFromEnv(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pools := map[string]*postgres.Pool{}
	for name, d := range map[string]string{
		"cached":  dsn,
		"unnamed": dsn + sep + "auto_cache_statements=false",
	} {
		p, err := postgres.Open(d, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open(%s): %v", name, err)
		}
		t.Cleanup(func() { _ = p.Close() })
		pools[name] = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, typ := range []string{"date", "timestamp", "timestamptz"} {
		for _, inf := range []string{"infinity", "-infinity"} {
			expr := "'" + inf + "'::" + typ
			for _, mode := range []string{"simple", "cached", "unnamed"} {
				t.Run(typ+"/"+inf+"/"+mode, func(t *testing.T) {
					p, q, args := pools["cached"], "SELECT "+expr, []any(nil)
					if mode != "simple" {
						q += " WHERE $1::int = 1"
						args = []any{1}
					}
					if mode == "unnamed" {
						p = pools["unnamed"]
					}
					var s string
					if err := p.QueryRow(ctx, q, args...).Scan(&s); err != nil || s != inf {
						t.Errorf("native *string: got %q err %v, want %q", s, err, inf)
					}
					// A *[]byte is the wire bytes (binary paths) or the server's text
					// (simple protocol), as in v1.5.11.
					var b []byte
					if err := p.QueryRow(ctx, q, args...).Scan(&b); err != nil {
						t.Errorf("native *[]byte: %v", err)
					} else if mode == "simple" && string(b) != inf {
						t.Errorf("native *[]byte (simple): got %q, want %q", b, inf)
					} else if mode != "simple" && string(b) == inf {
						t.Errorf("native *[]byte (%s): got the text %q, want the wire bytes", mode, b)
					}
				})
			}
		}
	}
}
