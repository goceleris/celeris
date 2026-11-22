//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// A role (or database) can set extra_float_digits and DateStyle. The text
// result format the driver asks for on the unnamed and simple paths depends
// on both: extra_float_digits <= 0 rounds float8 to 15 significant digits, and
// a non-ISO DateStyle prints dates and timestamps in a form the driver does
// not parse. The driver sends extra_float_digits=3 and DateStyle=ISO in the
// StartupMessage (which overrides a role or database setting) unless the DSN
// sets them itself.

// pinRole creates a login role that inherits the connection's password, with
// the given ALTER ROLE ... SET settings, and returns a DSN that connects as
// it. The test skips when the connecting role may not create roles.
func pinRole(t *testing.T, settings ...string) string {
	t.Helper()
	dsn := dsnFromEnv(t)
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		t.Skipf("skipping: CELERIS_PG_DSN is not a postgres:// URL with credentials")
	}
	admin := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	role := strings.ToLower(uniqueTableName(t, "pinrole"))
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

func TestStartupPinsFloatDigitsAgainstRoleSetting(t *testing.T) {
	base := pinRole(t, "extra_float_digits = 0")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const f8 = `0.30000000000000004::float8`
	const want = "0.30000000000000004"
	for name, dsn := range map[string]string{
		"cached":  base,
		"unnamed": withParams(base, "auto_cache_statements=false"),
	} {
		pool, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		for _, mode := range []string{"extended", "simple"} {
			if name == "cached" && mode == "simple" {
				continue // the simple path does not depend on the cache setting
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				q, args := "SELECT "+f8, []any(nil)
				if mode == "extended" {
					q += " WHERE $1::int = 1"
					args = []any{1}
				}
				var f float64
				if err := pool.QueryRow(ctx, q, args...).Scan(&f); err != nil || f != 0.30000000000000004 {
					t.Errorf("*float64: got %v err %v, want %s", f, err, want)
				}
				var s string
				if err := pool.QueryRow(ctx, q, args...).Scan(&s); err != nil || (name != "cached" && s != want) {
					t.Errorf("*string: got %q err %v", s, err)
				}
				var a any
				if err := pool.QueryRow(ctx, q, args...).Scan(&a); err != nil || a != any(0.30000000000000004) {
					t.Errorf("*any: got %#v err %v, want %s", a, err, want)
				}
			})
		}
	}

	// database/sql opens the same startup path.
	db, err := sql.Open(driverName, withParams(base, "auto_cache_statements=false"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var f float64
	if err := db.QueryRowContext(ctx, "SELECT "+f8+" WHERE $1::int = 1", 1).Scan(&f); err != nil || f != 0.30000000000000004 {
		t.Errorf("database/sql: got %v err %v, want %s", f, err, want)
	}
}

func TestStartupPinsDateStyleAgainstRoleSetting(t *testing.T) {
	base := pinRole(t, "DateStyle = 'SQL, DMY'")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for name, dsn := range map[string]string{
		"cached":  base,
		"unnamed": withParams(base, "auto_cache_statements=false"),
	} {
		pool, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		t.Run(name, func(t *testing.T) {
			var d, ts time.Time
			err := pool.QueryRow(ctx, "SELECT '2024-01-02'::date, '2024-01-02 03:04:05'::timestamp WHERE $1::int = 1", 1).Scan(&d, &ts)
			if err != nil {
				t.Fatalf("date/timestamp under a role DateStyle: %v", err)
			}
			if d.Format("2006-01-02") != "2024-01-02" || ts.Format("2006-01-02 15:04:05") != "2024-01-02 03:04:05" {
				t.Errorf("got %v and %v", d, ts)
			}
		})
	}
}

func TestStartupPinsByteaOutputAgainstRoleSetting(t *testing.T) {
	base := pinRole(t, "bytea_output = 'escape'")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	want := []byte{0x00, 0xff, 'a'}
	for name, dsn := range map[string]string{
		"cached":  base,
		"unnamed": withParams(base, "auto_cache_statements=false"),
	} {
		pool, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		for _, mode := range []string{"extended", "simple"} {
			if name == "cached" && mode == "simple" {
				continue
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				q, args := `SELECT '\x00ff61'::bytea`, []any(nil)
				if mode == "extended" {
					q += " WHERE $1::int = 1"
					args = []any{1}
				}
				var b []byte
				if err := pool.QueryRow(ctx, q, args...).Scan(&b); err != nil || string(b) != string(want) {
					t.Errorf("*[]byte: got %q err %v, want %q", b, err, want)
				}
				var a any
				if err := pool.QueryRow(ctx, q, args...).Scan(&a); err != nil {
					t.Errorf("*any: %v", err)
				} else if got, _ := a.([]byte); string(got) != string(want) {
					t.Errorf("*any: got %#v, want %q", a, want)
				}
			})
		}
	}
}

// A value the DSN sets itself wins over the driver's default.
func TestStartupPinsRespectExplicitDSN(t *testing.T) {
	dsn := dsnFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	show := func(t *testing.T, dsn, name string) string {
		t.Helper()
		pool, err := postgres.Open(dsn, postgres.WithMaxOpen(1))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		defer func() { _ = pool.Close() }()
		var v string
		if err := pool.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatalf("SHOW %s: %v", name, err)
		}
		return v
	}
	if got := show(t, dsn, "extra_float_digits"); got != "3" {
		t.Errorf("default extra_float_digits = %q, want 3", got)
	}
	if got := show(t, dsn, "DateStyle"); !strings.HasPrefix(got, "ISO") {
		t.Errorf("default DateStyle = %q, want ISO...", got)
	}
	if got := show(t, withParams(dsn, "extra_float_digits=1"), "extra_float_digits"); got != "1" {
		t.Errorf("explicit extra_float_digits = %q, want 1", got)
	}
	if got := show(t, withParams(dsn, "DateStyle=SQL"), "DateStyle"); !strings.HasPrefix(got, "SQL") {
		t.Errorf("explicit DateStyle = %q, want SQL...", got)
	}
	if got := show(t, dsn, "bytea_output"); got != "hex" {
		t.Errorf("default bytea_output = %q, want hex", got)
	}
	if got := show(t, withParams(dsn, "bytea_output=escape"), "bytea_output"); got != "escape" {
		t.Errorf("explicit bytea_output = %q, want escape", got)
	}
}
