//go:build postgres

package postgres_test

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"
)

// TestNativeScanVersionedTextTypes covers the native Pool.QueryContext /
// Pool.QueryRow Scan fast path against a real server, in both protocols:
//
//   - simple: a statement with no arguments, results come back as text;
//   - extended: a statement with arguments, results are requested in binary.
//
// A jsonb or jsonpath cell scanned into *[]byte, *string, *json.RawMessage or
// *any must be the value's text, never the binary wire form (a 0x01 version
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
				var b []byte
				if err := pool.QueryRow(ctx, q, args...).Scan(&b); err != nil || string(b) != c.want {
					t.Errorf("native *[]byte: got %q err %v, want %q", b, err, c.want)
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
