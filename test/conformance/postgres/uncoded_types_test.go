//go:build postgres

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// Types the driver has no codec for come back in the binary format on the
// extended protocol, so a *string or *[]byte would receive raw wire bytes. They
// are an error that names the type and says to cast to text; *any keeps what
// v1.5.11 returned (the raw bytes), and the text form through a cast works. The
// simple protocol (no arguments) reads text, where the text IS the value.
func TestUncodedTypesIntoStringAreAnError(t *testing.T) {
	cached := openSettingsPool(t, dsnFromEnv(t))
	unnamed := openSettingsPool(t, withParams(dsnFromEnv(t), "auto_cache_statements=false"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []struct{ typ, lit, text string }{
		{"macaddr", "'08:00:2b:01:02:03'", "08:00:2b:01:02:03"},
		{"point", "'(1,2)'", "(1,2)"},
		{"numeric[]", "'{1.5,2.50}'", "{1.5,2.50}"},
		{"date[]", "'{2025-01-15}'", "{2025-01-15}"},
		{"timestamptz[]", `'{"2025-01-15 12:00:00+00"}'`, `{"2025-01-15 12:00:00+00"}`},
	} {
		q := "SELECT " + c.lit + "::" + c.typ + " WHERE $1::int = 1"
		for path, pool := range map[string]*postgres.Pool{"cached": cached, "unnamed": unnamed} {
			t.Run(c.typ+"/"+path, func(t *testing.T) {
				baseName := strings.TrimSuffix(c.typ, "[]")
				var s string
				err := pool.QueryRow(ctx, q, 1).Scan(&s)
				if err == nil || !strings.Contains(err.Error(), baseName) || !strings.Contains(err.Error(), "::text") {
					t.Errorf("*string: got %q err %v, want an error naming %s and saying to cast to ::text", s, err, c.typ)
				}
				var b []byte
				err = pool.QueryRow(ctx, q, 1).Scan(&b)
				if err == nil || !strings.Contains(err.Error(), "::text") {
					t.Errorf("*[]byte: got %q err %v, want the same error", b, err)
				}
				var a any
				if err := pool.QueryRow(ctx, q, 1).Scan(&a); err != nil {
					t.Errorf("*any: %v", err)
				}
				// The cast to text works on every path.
				var txt string
				if err := pool.QueryRow(ctx, "SELECT ("+c.lit+"::"+c.typ+")::text WHERE $1::int = 1", 1).Scan(&txt); err != nil || txt != c.text {
					t.Errorf("::text: got %q err %v, want %q", txt, err, c.text)
				}
			})
		}
		// Simple protocol: the text is the value.
		t.Run(c.typ+"/simple", func(t *testing.T) {
			var s string
			if err := cached.QueryRow(ctx, "SELECT "+c.lit+"::"+c.typ).Scan(&s); err != nil || s != c.text {
				t.Errorf("simple *string: got %q err %v, want %q", s, err, c.text)
			}
		})
	}
}

// Types with a binary-to-text codec (oid, ...): the extended protocol's binary
// result decodes to the server's text, into *string and *[]byte (and *any holds
// the same text as a string; oid, a number, holds an int64), where the type was
// an error or raw bytes before. The simple protocol is untouched: it reads the
// server's text, and *any there is that text as []byte, as in v1.5.11.
func TestTextCodecTypesIntoString(t *testing.T) {
	cached := openSettingsPool(t, dsnFromEnv(t))
	unnamed := openSettingsPool(t, withParams(dsnFromEnv(t), "auto_cache_statements=false"))
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []struct {
		typ, lit, text string
		anyWant        any
	}{
		{"oid", "4294967295", "4294967295", int64(4294967295)},
		{"time", "'12:34:56.789'", "12:34:56.789", "12:34:56.789"},
		{"timetz", "'12:34:56.789+05:30'", "12:34:56.789+05:30", "12:34:56.789+05:30"},
		{"inet", "'192.168.1.5/24'", "192.168.1.5/24", "192.168.1.5/24"},
		{"inet", "'10.0.0.1/32'", "10.0.0.1", "10.0.0.1"},
		{"cidr", "'10.0.0.0/8'", "10.0.0.0/8", "10.0.0.0/8"},
		{"cidr", "'10.0.0.1/32'", "10.0.0.1/32", "10.0.0.1/32"},
		{"interval", "'1 year 2 mons -3 days 04:05:06.789'", "1 year 2 mons -3 days +04:05:06.789", "1 year 2 mons -3 days +04:05:06.789"},
	} {
		q := "SELECT " + c.lit + "::" + c.typ + " WHERE $1::int = 1"
		for path, pool := range map[string]*postgres.Pool{"cached": cached, "unnamed": unnamed} {
			t.Run(c.typ+"/"+path, func(t *testing.T) {
				var s string
				if err := pool.QueryRow(ctx, q, 1).Scan(&s); err != nil || s != c.text {
					t.Errorf("*string: got %q err %v, want %q", s, err, c.text)
				}
				var b []byte
				if err := pool.QueryRow(ctx, q, 1).Scan(&b); err != nil || string(b) != c.text {
					t.Errorf("*[]byte: got %q err %v, want %q", b, err, c.text)
				}
				var a any
				if err := pool.QueryRow(ctx, q, 1).Scan(&a); err != nil || a != c.anyWant {
					t.Errorf("*any: got %#v err %v, want %#v", a, err, c.anyWant)
				}
			})
		}
		t.Run(c.typ+"/database-sql", func(t *testing.T) {
			var s string
			if err := db.QueryRowContext(ctx, q, 1).Scan(&s); err != nil || s != c.text {
				t.Errorf("database/sql *string: got %q err %v, want %q", s, err, c.text)
			}
		})
		t.Run(c.typ+"/simple", func(t *testing.T) {
			var s string
			if err := cached.QueryRow(ctx, "SELECT "+c.lit+"::"+c.typ).Scan(&s); err != nil || s != c.text {
				t.Errorf("simple *string: got %q err %v, want %q", s, err, c.text)
			}
			var a any
			if err := cached.QueryRow(ctx, "SELECT "+c.lit+"::"+c.typ).Scan(&a); err != nil {
				t.Fatalf("simple *any: %v", err)
			}
			if b, ok := a.([]byte); !ok || string(b) != c.text {
				t.Errorf("simple *any: got %#v, want the server's text %q as []byte (v1.5.11)", a, c.text)
			}
		})
	}
}

// A NULL of such a type is a NULL, not an error, and a multi-dimensional array
// is a clear error, not a flattened one.
func TestUncodedTypesNullAndMultiDimArray(t *testing.T) {
	pool := openSettingsPool(t, dsnFromEnv(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := "STALE"
	if err := pool.QueryRow(ctx, "SELECT NULL::interval WHERE $1::int = 1", 1).Scan(&s); err != nil || s != "" {
		t.Errorf("NULL interval into *string: got %q err %v, want \"\"", s, err)
	}
	var a any
	err := pool.QueryRow(ctx, "SELECT ARRAY[[1,2],[3,4]]::int4[] WHERE $1::int = 1", 1).Scan(&a)
	if err == nil || !strings.Contains(err.Error(), "multi-dimensional") {
		t.Errorf("2-D array: got %v err %v, want a multi-dimensional error", a, err)
	}
}
