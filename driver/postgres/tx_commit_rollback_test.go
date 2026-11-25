package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// PostgreSQL answers COMMIT with the command tag ROLLBACK (and no error) when the
// transaction is in the aborted state. Commit must report it
// (ErrTxCommitRollback), on database/sql and on the native Pool in direct mode
// and on the event loop, and the conn must be free again.

type txFakePG struct {
	addr     string
	mu       sync.Mutex
	sessions int
}

func (f *txFakePG) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func startTxFakePG(t *testing.T) *txFakePG {
	t.Helper()
	f := &txFakePG{}
	f.addr = startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		if !readStartup(t, c) {
			return
		}
		f.mu.Lock()
		f.sessions++
		f.mu.Unlock()
		if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
			return
		}
		status := byte('I')
		for {
			typ, payload, err := readMsg(c)
			if err != nil || typ == protocol.MsgTerminate {
				return
			}
			if typ != protocol.MsgQuery {
				continue
			}
			switch string(bytes.TrimRight(payload, "\x00")) {
			case "BEGIN":
				status = 'T'
				_ = writeCommandComplete(c, "BEGIN")
			case "SELECT bad":
				status = 'E'
				_ = writeMsg(c, protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C22012\x00Mdivision by zero\x00\x00"))
			case "COMMIT":
				tag := "COMMIT"
				if status == 'E' {
					tag = "ROLLBACK"
				}
				status = 'I'
				_ = writeCommandComplete(c, tag)
			case "ROLLBACK":
				status = 'I'
				_ = writeCommandComplete(c, "ROLLBACK")
			default:
				_ = writeCommandComplete(c, "SELECT 1")
			}
			_ = writeReadyForQuery(c, status)
		}
	})
	return f
}

type txArm struct {
	name string
	// begin starts a transaction on a single conn and returns its operations.
	begin func(t *testing.T, addr string) (exec func(q string) error, commit func() error, next func() error)
}

func txArms() []txArm {
	dsn := func(addr string) string {
		host, port, _ := net.SplitHostPort(addr)
		return "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
	}
	poolArm := func(name string, opts ...Option) txArm {
		return txArm{name, func(t *testing.T, addr string) (func(string) error, func() error, func() error) {
			p, err := Open(dsn(addr), append([]Option{WithMaxOpen(1)}, opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			ctx := context.Background()
			tx, err := p.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			return func(q string) error { _, err := tx.ExecContext(ctx, q); return err },
				tx.Commit,
				func() error { _, err := p.ExecContext(ctx, "SELECT next"); return err }
		}}
	}
	return []txArm{
		poolArm("pool-direct"),
		poolArm("pool-loop", WithEngine(looptest.Unstarted())),
		{"database-sql", func(t *testing.T, addr string) (func(string) error, func() error, func() error) {
			db, err := sql.Open(DriverName, dsn(addr))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			return func(q string) error { _, err := tx.ExecContext(ctx, q); return err },
				tx.Commit,
				func() error { _, err := db.ExecContext(ctx, "SELECT next"); return err }
		}},
	}
}

func TestCommitOfAnAbortedTransactionReportsTheRollback(t *testing.T) {
	for _, arm := range txArms() {
		t.Run(arm.name, func(t *testing.T) {
			f := startTxFakePG(t)
			exec, commit, next := arm.begin(t, f.addr)
			if err := exec("SELECT bad"); err == nil {
				t.Fatal("the failing statement returned no error")
			}
			if err := commit(); !errors.Is(err, ErrTxCommitRollback) {
				t.Fatalf("Commit() = %v, want ErrTxCommitRollback: the server rolled the transaction back", err)
			}
			// The transaction is over and the conn is free: with MaxOpen 1 the next
			// statement runs on it.
			done := make(chan error, 1)
			go func() { done <- next() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the statement after Commit failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the conn was not released by Commit")
			}
			if got := f.sessionCount(); got != 1 {
				t.Fatalf("sessions = %d, want 1: the conn is reused", got)
			}
		})
	}
}

func TestCommitOfAHealthyTransactionStillSucceeds(t *testing.T) {
	for _, arm := range txArms() {
		t.Run(arm.name, func(t *testing.T) {
			f := startTxFakePG(t)
			exec, commit, next := arm.begin(t, f.addr)
			if err := exec("SELECT ok"); err != nil {
				t.Fatal(err)
			}
			if err := commit(); err != nil {
				t.Fatalf("Commit() = %v, want nil", err)
			}
			if err := next(); err != nil {
				t.Fatalf("the statement after Commit failed: %v", err)
			}
		})
	}
}
