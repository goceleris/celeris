//go:build postgres

package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// envPoolerDSN is the DSN of a PgBouncer (default configuration: no
// ignore_startup_parameters) in front of the test database. The CI compose file
// has no pooler, so the test skips without it; the manual check is in the PR that
// added it:
//
//	CELERIS_PGBOUNCER_DSN=postgres://celeris:celeris@localhost:6432/celeristest?sslmode=disable
const envPoolerDSN = "CELERIS_PGBOUNCER_DSN"

// A pooler answers a startup parameter it does not track with a FATAL error
// and closes the connection (PgBouncer 1.26.1: "unsupported startup parameter:
// extra_float_digits"). The driver's startup pins must not make it unreachable.
func TestPoolerRefusingStartupPins(t *testing.T) {
	dsn := os.Getenv(envPoolerDSN)
	if dsn == "" {
		t.Skipf("skipping: %s is not set", envPoolerDSN)
	}
	for name, d := range map[string]string{
		"default":          dsn,
		"startup_pins=off": withParams(dsn, "startup_pins=off"),
		"unnamed":          withParams(dsn, "auto_cache_statements=false"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			pool, err := postgres.Open(d, postgres.WithMaxOpen(2))
			if err != nil {
				t.Fatalf("postgres.Open: %v", err)
			}
			defer func() { _ = pool.Close() }()
			var ds string
			if err := pool.QueryRow(ctx, "SELECT current_setting('DateStyle')").Scan(&ds); err != nil {
				t.Fatalf("query through the pooler: %v", err)
			}
			// PgBouncer tracks DateStyle, so the pin is applied through it; with
			// startup_pins=off the role's or the database's own setting stays.
			if name != "startup_pins=off" && !strings.HasPrefix(ds, "ISO") {
				t.Errorf("DateStyle = %q, want ISO...", ds)
			}
			var b []byte
			if err := pool.QueryRow(ctx, `SELECT '\x00ff61'::bytea`).Scan(&b); err != nil || string(b) != "\x00\xffa" {
				t.Errorf("bytea: %q, %v", b, err)
			}
		})
	}
}
