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
// are an error that names the type and says to cast to text when scanned into a
// *string; *[]byte and *any keep what v1.5.11 returned (the wire bytes), and the text form through a cast works. The
// simple protocol (no arguments) reads text, where the text IS the value.
func TestUncodedTypesIntoStringAreAnError(t *testing.T) {
	cached := openSettingsPool(t, dsnFromEnv(t))
	unnamed := openSettingsPool(t, withParams(dsnFromEnv(t), "auto_cache_statements=false"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []struct{ typ, lit, text string }{
		{"time", "'12:34:56.789'", "12:34:56.789"},
		{"timetz", "'12:34:56.789+05:30'", "12:34:56.789+05:30"},
		{"interval", "'1 day 02:03:04'", "1 day 02:03:04"},
		{"inet", "'192.168.1.5/24'", "192.168.1.5/24"},
		{"cidr", "'10.0.0.0/8'", "10.0.0.0/8"},
		{"macaddr", "'08:00:2b:01:02:03'", "08:00:2b:01:02:03"},
		{"oid", "4294967295", "4294967295"},
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
				if err := pool.QueryRow(ctx, q, 1).Scan(&b); err != nil || len(b) == 0 {
					t.Errorf("*[]byte: got %q err %v, want the wire bytes (as v1.5.11)", b, err)
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
