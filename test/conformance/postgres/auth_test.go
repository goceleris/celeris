//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// These auth tests are intentionally lenient. A single CELERIS_PG_DSN env var
// points at one pg instance configured with one auth method — so we test
// whatever that happens to be. Exercising MD5 / SCRAM-SHA-256 / Trust /
// Cleartext on the same run would require either multiple DSN env vars or a
// multi-role server with pg_hba rules per user. We opt for the simpler design
// and document the limitation.
//
// To exercise each method explicitly, run this suite multiple times against a
// pg configured differently — see docker-compose.yml in this directory for the
// default SCRAM-SHA-256 setup and inline notes on switching methods.

// TestAuthHandshakeSucceeds is the smoke test: if our Connect against the DSN
// went through, the handshake worked. SCRAM / MD5 / cleartext differences
// live in protocol/startup.go and are unit-tested there.
func TestAuthHandshakeSucceeds(t *testing.T) {
	db := openDB(t)
	// The openDB helper already does PingContext; if we reach here, auth
	// passed. Take the opportunity to log what the server advertised as the
	// authentication method so regressions are obvious in CI logs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var method sql.NullString
	// password_encryption is the server's preferred storage method, which
	// implies the SASL mechanism it will demand on future connects.
	if err := db.QueryRowContext(ctx, "SHOW password_encryption").Scan(&method); err != nil {
		t.Logf("could not SHOW password_encryption: %v", err)
		return
	}
	t.Logf("server password_encryption = %q (SCRAM-SHA-256 / md5)", method.String)
}

// TestReconnectAfterClose exercises the happy-path reconnect: close the
// sql.DB handle, reopen, re-ping. Any auth-state leakage would show up here
// as a failure on the second ping.
func TestReconnectAfterClose(t *testing.T) {
	dsn := dsnFromEnv(t)
	for i := 0; i < 3; i++ {
		db, err := sql.Open(driverName, dsn)
		if err != nil {
			t.Fatalf("Open[%d]: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := db.PingContext(ctx); err != nil {
			cancel()
			_ = db.Close()
			t.Fatalf("Ping[%d]: %v", i, err)
		}
		cancel()
		_ = db.Close()
	}
}
