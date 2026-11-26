//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// On a direct connection (the default of database/sql, and a Pool without an
// event loop) NOTIFY works, plain, with a payload and inside a transaction; LISTEN
// and UNLISTEN return ErrDirectModeUnsupported, whose text names
// Connector.WithStreaming and the streaming=true DSN parameter. The driver has no
// way to receive a notification on any path, so none is received here.
func TestDirectNotifyWorksListenIsRefused(t *testing.T) {
	dsn := dsnFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	checkListenErr := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, postgres.ErrDirectModeUnsupported) {
			t.Fatalf("err = %v, want ErrDirectModeUnsupported", err)
		}
		for _, want := range []string{"Connector.WithStreaming", "streaming=true"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error %q does not name %q", err, want)
			}
		}
	}

	t.Run("database/sql", func(t *testing.T) {
		db, err := sql.Open(driverName, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		for _, q := range []string{"NOTIFY wd_notify", "NOTIFY wd_notify, 'payload'", "notify wd_notify;", "SELECT pg_notify('wd_notify', 'p')"} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "NOTIFY wd_notify, 'in a transaction'"); err != nil {
			t.Errorf("NOTIFY in a transaction: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("Commit after NOTIFY: %v", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		for _, q := range []string{"LISTEN wd_notify", "UNLISTEN wd_notify", "UNLISTEN *"} {
			_, err := conn.ExecContext(ctx, q)
			checkListenErr(t, err)
			if rows, err := conn.QueryContext(ctx, q); err == nil {
				_ = rows.Close()
				t.Errorf("Query %s: no error", q)
			} else {
				checkListenErr(t, err)
			}
		}
		var one int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Fatalf("the connection after the refused LISTEN: %d, %v", one, err)
		}
	})

	t.Run("native Pool", func(t *testing.T) {
		p, err := postgres.Open(dsn, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		for _, q := range []string{"NOTIFY wd_notify", "NOTIFY wd_notify, 'payload'", "SELECT pg_notify('wd_notify', 'p')"} {
			if _, err := p.ExecContext(ctx, q); err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "NOTIFY wd_notify, 'in a transaction'"); err != nil {
			t.Errorf("NOTIFY in a transaction: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("Commit after NOTIFY: %v", err)
		}
		for _, q := range []string{"LISTEN wd_notify", "UNLISTEN wd_notify"} {
			_, err := p.ExecContext(ctx, q)
			checkListenErr(t, err)
		}
		var one int
		if err := p.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Fatalf("the pool after the refused LISTEN: %d, %v", one, err)
		}
	})

	t.Run("WithStreaming accepts LISTEN as before", func(t *testing.T) {
		cn, err := postgres.NewConnector(dsn)
		if err != nil {
			t.Fatal(err)
		}
		db := sql.OpenDB(cn.WithStreaming())
		defer func() { _ = db.Close() }()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.ExecContext(ctx, "LISTEN wd_notify"); err != nil {
			t.Errorf("LISTEN on the loop: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "NOTIFY wd_notify, 'x'"); err != nil {
			t.Errorf("NOTIFY on the loop: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "UNLISTEN wd_notify"); err != nil {
			t.Errorf("UNLISTEN on the loop: %v", err)
		}
	})
}
