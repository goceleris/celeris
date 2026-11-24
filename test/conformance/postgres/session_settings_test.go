//go:build postgres

package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A role (or database) can change how the server prints a result in the text
// format, which the simple protocol and the unnamed extended statement use. The
// driver does not set these at connect (it connects as v1.5.11 did, also behind
// poolers that refuse unknown startup parameters); it reads what it can:
// bytea_output (hex and escape), DateStyle (ISO styles, a clear error for the
// others). extra_float_digits cannot be read: see issue 1364.

// settingsRole creates a login role that inherits the connection's password,
// with the given ALTER ROLE ... SET settings, and returns a DSN that connects
// as it. The test skips when the connecting role may not create roles.
func settingsRole(t *testing.T, settings ...string) string {
	t.Helper()
	dsn := dsnFromEnv(t)
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		t.Skipf("skipping: CELERIS_PG_DSN is not a postgres:// URL with credentials")
	}
	admin := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	role := strings.ToLower(uniqueTableName(t, "setrole"))
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'pin'", role)); err != nil {
		t.Skipf("skipping: cannot create a role: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP ROLE IF EXISTS " + role) })
	for _, s := range settings {
		if _, err := admin.ExecContext(ctx, fmt.Sprintf("ALTER ROLE %s SET %s", role, s)); err != nil {
			t.Fatalf("ALTER ROLE SET %s: %v", s, err)
		}
	}
	u.User = url.UserPassword(role, "pin")
	return u.String()
}

func withParams(dsn string, params ...string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(params, "&")
}

// arrText renders an array cell: the text of a []byte, fmt.Sprint otherwise.
func arrText(a any) string {
	if b, ok := a.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(a)
}

func openSettingsPool(t *testing.T, dsn string) *postgres.Pool {
	t.Helper()
	p, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// bytea_output=escape: the bytes read back on every path, bytea[] included.
func TestSessionByteaOutputEscapeRole(t *testing.T) {
	base := settingsRole(t, "bytea_output = 'escape'")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for name, dsn := range map[string]string{"cached": base, "unnamed": withParams(base, "auto_cache_statements=false")} {
		pool := openSettingsPool(t, dsn)
		for _, mode := range []string{"extended", "simple"} {
			if name == "cached" && mode == "simple" {
				continue
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				suffix, args := "", []any(nil)
				if mode == "extended" {
					suffix, args = " WHERE $1::int = 1", []any{1}
				}
				var b []byte
				if err := pool.QueryRow(ctx, `SELECT '\x00ff5c7a'::bytea`+suffix, args...).Scan(&b); err != nil || string(b) != "\x00\xff\\z" {
					t.Errorf("bytea: got %q err %v", b, err)
				}
				var a any
				if err := pool.QueryRow(ctx, `SELECT ARRAY['\x01ff'::bytea]`+suffix, args...).Scan(&a); err != nil {
					t.Errorf("bytea[]: %v", err)
				} else if got := arrText(a); got != "[[1 255]]" && got != `{"\\001\\377"}` {
					// The binary path decodes the array; a text path hands back
					// the server's array text (no array text decoder), which is
					// the escape form double-escaped inside the array quoting.
					t.Errorf("bytea[]: got %#v, want [[1 255]] or the server's text", a)
				}
			})
		}
	}
}

// DateStyle='SQL, DMY' (non-ISO): the text paths return an error that names
// DateStyle, never a wrong date; the binary (cached) path is unaffected.
func TestSessionNonISODateStyleRole(t *testing.T) {
	base := settingsRole(t, "DateStyle = 'SQL, DMY'")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const q = "SELECT '2024-01-02'::date, '2024-01-02 03:04:05'::timestamp, '2024-01-02 03:04:05+00'::timestamptz"
	want := [3]string{"2024-01-02 00:00:00", "2024-01-02 03:04:05", "2024-01-02 03:04:05"}

	cached := openSettingsPool(t, base)
	t.Run("cached", func(t *testing.T) {
		var d, ts, tz time.Time
		if err := cached.QueryRow(ctx, q+" WHERE $1::int = 1", 1).Scan(&d, &ts, &tz); err != nil {
			t.Fatalf("cached (binary) path: %v", err)
		}
		for i, got := range []time.Time{d, ts, tz} {
			if g := got.UTC().Format("2006-01-02 15:04:05"); g != want[i] {
				t.Errorf("column %d: got %s, want %s", i, g, want[i])
			}
		}
	})
	unnamed := openSettingsPool(t, withParams(base, "auto_cache_statements=false"))
	for name, run := range map[string]func() error{
		"unnamed": func() error {
			var d, ts, tz time.Time
			return unnamed.QueryRow(ctx, q+" WHERE $1::int = 1", 1).Scan(&d, &ts, &tz)
		},
		"simple": func() error {
			var d, ts, tz time.Time
			return cached.QueryRow(ctx, q).Scan(&d, &ts, &tz)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "DateStyle") {
				t.Errorf("want an error naming DateStyle, got %v", err)
			}
		})
	}
}

// Known limit (issue 1364): a role with extra_float_digits=0 makes the text
// format print 15 significant digits, so float8 0.30000000000000004 reads back
// as 0.3 on the simple and unnamed paths. The driver cannot read the setting.
// The test documents the limit: it asserts the exact value on the cached
// (binary) path and skips the text paths with the issue number.
func TestSessionExtraFloatDigitsZeroRoleKnownLimit(t *testing.T) {
	base := settingsRole(t, "extra_float_digits = 0")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const q = "SELECT 0.30000000000000004::float8"
	cached := openSettingsPool(t, base)
	var f float64
	if err := cached.QueryRow(ctx, q+" WHERE $1::int = 1", 1).Scan(&f); err != nil || f != 0.30000000000000004 {
		t.Errorf("cached (binary): got %v err %v", f, err)
	}
	t.Run("text-paths", func(t *testing.T) {
		t.Skip("known limit, goceleris/celeris#1364: extra_float_digits<=0 loses digits on the simple and unnamed paths; set it to 1 or more on the role or database")
	})
}
