//go:build postgres && pgbouncer

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// PgBouncer conformance: the driver must connect through a connection pooler
// exactly like v1.5.11 did. A pooler answers a startup parameter it does not
// track (extra_float_digits, bytea_output, ...) with a FATAL 08P01 and closes
// the connection, so a driver that sends one is unreachable behind PgBouncer.
//
// The Drivers workflow runs this against PgBouncer (pool modes session and
// transaction, see pgbouncer/pgbouncer.ini) in front of each Postgres service,
// with -tags 'postgres pgbouncer'. The extra tag keeps it out of the
// Driver Conformance job (-tags postgres), which has no PgBouncer and fails on
// a skipped test. It skips cleanly unless CELERIS_PGBOUNCER_DSN (the
// transaction-mode pool) is set; CELERIS_PGBOUNCER_SESSION_DSN adds the
// session-mode pool.
const (
	envPgBouncerDSN        = "CELERIS_PGBOUNCER_DSN"
	envPgBouncerSessionDSN = "CELERIS_PGBOUNCER_SESSION_DSN"
)

func pgbouncerDSNs(t *testing.T) map[string]string {
	t.Helper()
	txn := strings.TrimSpace(os.Getenv(envPgBouncerDSN))
	if txn == "" {
		t.Skipf("skipping: %s not set (set it to a DSN that connects through PgBouncer)", envPgBouncerDSN)
	}
	out := map[string]string{"transaction": txn}
	if s := strings.TrimSpace(os.Getenv(envPgBouncerSessionDSN)); s != "" {
		out["session"] = s
	}
	return out
}

func bytesEqualAny(a any, want []byte) bool {
	b, ok := a.([]byte)
	return ok && string(b) == string(want)
}

func pgbouncerParamSep(dsn string) string {
	if strings.Contains(dsn, "?") {
		return "&"
	}
	return "?"
}

func TestPgBouncerRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 22, 12, 34, 56, 789012000, time.UTC)
	for pool, base := range pgbouncerDSNs(t) {
		for _, path := range []string{"simple", "cached", "unnamed"} {
			t.Run(pool+"/"+path, func(t *testing.T) {
				dsn := base
				if path == "unnamed" {
					dsn = base + pgbouncerParamSep(base) + "auto_cache_statements=false"
				}
				p, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
				if err != nil {
					t.Fatalf("postgres.Open through PgBouncer: %v", err)
				}
				t.Cleanup(func() { _ = p.Close() })
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.Ping(ctx); err != nil {
					t.Fatalf("Ping through PgBouncer: %v", err)
				}

				// simple: literals, no arguments; cached and unnamed: arguments.
				q := `SELECT 'héllo'::text, 9007199254740993::int8, 0.30000000000000004::float8, true,
					'\x00ff10'::bytea, '2026-11-22 12:34:56.789012+00'::timestamptz, ('{"a": 1}'::jsonb)::text`
				var args []any
				if path != "simple" {
					q = `SELECT $1::text, $2::int8, $3::float8, $4::bool, $5::bytea, $6::timestamptz, ($7::jsonb)::text`
					args = []any{"héllo", int64(9007199254740993), 0.30000000000000004, true, []byte{0, 0xff, 0x10}, ts, `{"a": 1}`}
				}
				var (
					s     string
					i     int64
					f     float64
					b     bool
					by    any // bytea read through *any: the decoded []byte on every path
					tm    time.Time
					jtext string
				)
				// Twice: the second call reuses a cached statement (cached) or
				// a fresh unnamed one, and a pooled server connection.
				for round := 0; round < 2; round++ {
					if err := p.QueryRow(ctx, q, args...).Scan(&s, &i, &f, &b, &by, &tm, &jtext); err != nil {
						t.Fatalf("round %d: %v", round, err)
					}
					if s != "héllo" || i != 9007199254740993 || f != 0.30000000000000004 || !b ||
						!bytesEqualAny(by, []byte{0, 0xff, 0x10}) || !tm.Equal(ts) {
						t.Errorf("round %d: got %q %d %v %v %q %v", round, s, i, f, b, by, tm)
					}
					var j map[string]any
					if err := json.Unmarshal([]byte(jtext), &j); err != nil || j["a"] != float64(1) {
						t.Errorf("round %d: jsonb text %q: %v", round, jtext, err)
					}
				}
			})
		}

		t.Run(pool+"/database-sql", func(t *testing.T) {
			db, err := sql.Open(postgres.DriverName, base)
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var n int64
			if err := db.QueryRowContext(ctx, "SELECT $1::int8 + 1", int64(41)).Scan(&n); err != nil || n != 42 {
				t.Fatalf("database/sql through PgBouncer: got %d err %v", n, err)
			}
		})
	}
}
