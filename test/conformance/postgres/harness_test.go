//go:build postgres

// Package postgres_test exercises the celeris PostgreSQL driver against a
// real server. It is gated by both a `postgres` build tag and the
// CELERIS_PG_DSN environment variable; with either missing the tests skip
// cleanly so `go test ./...` remains green on a vanilla checkout.
//
// To run:
//
//	CELERIS_PG_DSN=postgres://celeris:celeris@localhost:5432/celeristest?sslmode=disable \
//	  go test -tags postgres ./test/conformance/postgres/...
//
// See docker-compose.yml in this directory for a disposable server.
package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/goceleris/celeris/driver/postgres"
)

const (
	// envDSN is the env var holding the connection string. Tests skip if empty.
	envDSN = "CELERIS_PG_DSN"
	// driverName matches driver.DriverName; inlined to avoid re-importing the
	// package just for the constant.
	driverName = "celeris-postgres"
)

// tableCounter gives each generated table name a monotonic suffix so parallel
// subtests never collide even with the same random prefix.
var tableCounter uint64

// dsnFromEnv returns the DSN or skips the test. Calling this is the standard
// gate at the top of every conformance test.
func dsnFromEnv(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envDSN))
	if dsn == "" {
		t.Skipf("skipping: %s not set (set it to a reachable postgres DSN to run)", envDSN)
	}
	return dsn
}

// openDB opens a database/sql handle, pings, and registers t.Cleanup.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := dsnFromEnv(t)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Keep the pool small so a misbehaving test doesn't hoard server slots.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("skipping: PingContext failed (server unreachable?): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// uniqueTableName returns a fresh identifier in the form
// "celeris_test_<rand>_<counter>_<label>" and enforces the 63-byte PG
// identifier limit by truncating rather than erroring out.
func uniqueTableName(t *testing.T, label string) string {
	t.Helper()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	n := atomic.AddUint64(&tableCounter, 1)
	name := fmt.Sprintf("celeris_test_%s_%d_%s", hex.EncodeToString(buf), n, sanitizeLabel(label))
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// sanitizeLabel normalizes a human label into a legal unquoted PG identifier
// suffix: lowercase, alphanumeric, underscore.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "t"
	}
	return b.String()
}

// createTable runs CREATE TABLE and schedules DROP TABLE IF EXISTS on cleanup.
// The caller passes the column list exactly as it would appear between the
// parentheses of CREATE TABLE (e.g. `id int primary key, body text`).
func createTable(t *testing.T, db *sql.DB, name, cols string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (%s)", name, cols))
	if err != nil {
		t.Fatalf("CREATE TABLE %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+name)
	})
}

// serverVersionNum returns the Postgres server_version_num GUC as an integer
// (e.g. 160004 for 16.4). Returns 0 if the query fails.
func serverVersionNum(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SHOW server_version_num").Scan(&n); err != nil {
		// Older servers return the numeric in text form; try to parse it.
		var s string
		if err2 := db.QueryRowContext(ctx, "SHOW server_version_num").Scan(&s); err2 == nil {
			_, _ = fmt.Sscanf(s, "%d", &n)
		}
	}
	return n
}

// requireVersion skips the test if the server version is below min.
func requireVersion(t *testing.T, db *sql.DB, min int) {
	t.Helper()
	if v := serverVersionNum(t, db); v != 0 && v < min {
		t.Skipf("skipping: server_version_num=%d < required %d", v, min)
	}
}
