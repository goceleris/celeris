package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A call to Commit or Rollback ends the native Tx whatever it returns: every later
// call on the Tx returns ErrTxDone and sends nothing, and its conn is released
// exactly once: back to the pool when the driver knows it is idle and outside a
// transaction (the reply was read up to ReadyForQuery), discarded otherwise.

// endFakePG is a fake server whose COMMIT and ROLLBACK answers are scripted, and
// which records every query it receives.
type endFakePG struct {
	addr string
	// commit and rollback: "ok", "err-idle" (ErrorResponse 40001, then ReadyForQuery I),
	// "err-open" (ErrorResponse, then ReadyForQuery T: a transaction still open),
	// "err-failed" (ErrorResponse, then ReadyForQuery E), "close" (the socket is closed
	// when the statement arrives).
	commit, rollback string

	mu       sync.Mutex
	sessions int
	queries  []string
}

func (f *endFakePG) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func (f *endFakePG) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func startEndFakePG(t *testing.T, commit, rollback string) *endFakePG {
	t.Helper()
	f := &endFakePG{commit: commit, rollback: rollback}
	errResp := []byte("SERROR\x00VERROR\x00C40001\x00Mcould not serialize access\x00\x00")
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
			q := string(bytes.TrimRight(payload, "\x00"))
			f.mu.Lock()
			f.queries = append(f.queries, q)
			f.mu.Unlock()
			script := func(mode, tag string) bool {
				switch mode {
				case "close":
					return false
				case "err-idle":
					status = 'I'
					_ = writeMsg(c, protocol.BackendErrorResponse, errResp)
				case "err-open":
					status = 'T'
					_ = writeMsg(c, protocol.BackendErrorResponse, errResp)
				case "err-failed":
					status = 'E'
					_ = writeMsg(c, protocol.BackendErrorResponse, errResp)
				default:
					status = 'I'
					_ = writeCommandComplete(c, tag)
				}
				return true
			}
			switch q {
			case "BEGIN":
				status = 'T'
				_ = writeCommandComplete(c, "BEGIN")
			case "COMMIT":
				if !script(f.commit, "COMMIT") {
					return
				}
			case "ROLLBACK":
				if !script(f.rollback, "ROLLBACK") {
					return
				}
			default:
				_ = writeCommandComplete(c, "SELECT 1")
			}
			_ = writeReadyForQuery(c, status)
		}
	})
	return f
}

func openEndPool(t *testing.T, addr string, loop bool) *Pool {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	opts := []Option{WithMaxOpen(1)}
	if loop {
		opts = append(opts, WithEngine(looptest.Unstarted()))
	}
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// beginBounded is BeginTx that fails the test instead of hanging when the pool has
// no free slot (a leaked conn: MaxOpen 1).
func beginBounded(t *testing.T, p *Pool, what string) *Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s: BeginTx: %v (stats %+v): the conn of the previous Tx was not released", what, err, p.Stats())
	}
	return tx
}

// assertTxEnded checks that every method of tx now returns ErrTxDone, and that none
// of them sent anything to the server. finishers=false leaves out the second Commit and
// the Rollback, which on a driver that held the conn after a failed Commit are what freed
// it: a test of "the caller never rolls back" must not call them.
func assertTxEnded(t *testing.T, tx *Tx, f *endFakePG, what string, finishers bool) {
	t.Helper()
	ctx := context.Background()
	before := len(f.seen())
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrTxDone) || !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("%s: %s = %v, want ErrTxDone (and sql.ErrTxDone)", what, name, err)
		}
	}
	_, err := tx.ExecContext(ctx, "SELECT ok")
	check("ExecContext", err)
	_, err = tx.QueryContext(ctx, "SELECT ok")
	check("QueryContext", err)
	check("QueryRow.Scan", tx.QueryRow(ctx, "SELECT ok").Scan(new(int64)))
	check("Savepoint", tx.Savepoint(ctx, "sp"))
	check("ReleaseSavepoint", tx.ReleaseSavepoint(ctx, "sp"))
	check("RollbackToSavepoint", tx.RollbackToSavepoint(ctx, "sp"))
	if finishers {
		check("Commit (again)", tx.Commit())
		check("Rollback", tx.Rollback())
	}
	if got := f.seen(); len(got) != before {
		t.Errorf("%s: the ended Tx sent %v to the server", what, got[before:])
	}
}

func TestTxCommitEndsTheTx(t *testing.T) {
	type cell struct {
		name string
		// commit / rollback script of the fake server
		commit, rollback string
		// reused: the conn goes back to the pool (it is idle, outside a
		// transaction); otherwise it is discarded and replaced
		reused bool
		// failCommit: Commit returns an error
		failCommit bool
	}
	cells := []cell{
		{"commit-error-idle", "err-idle", "ok", true, true},
		{"commit-socket-closed", "close", "ok", false, true},
		{"commit-error-tx-still-open", "err-open", "ok", false, true},
		{"commit-error-tx-failed", "err-failed", "ok", false, true},
		{"commit-ok", "ok", "ok", true, false},
	}
	for _, loop := range []bool{false, true} {
		name := "direct"
		if loop {
			name = "loop"
		}
		for _, c := range cells {
			for _, withSavepoint := range []bool{false, true} {
				for _, rollbackAfter := range []bool{false, true} {
					label := name + "/" + c.name
					if withSavepoint {
						label += "/savepoint"
					}
					if rollbackAfter {
						label += "/then-rollback"
					}
					t.Run(label, func(t *testing.T) {
						f := startEndFakePG(t, c.commit, c.rollback)
						p := openEndPool(t, f.addr, loop)
						ctx := context.Background()
						const rounds = 5
						for i := 0; i < rounds; i++ {
							tx := beginBounded(t, p, label)
							if _, err := tx.ExecContext(ctx, "SELECT ok"); err != nil {
								t.Fatal(err)
							}
							if withSavepoint {
								if err := tx.Savepoint(ctx, "sp"); err != nil {
									t.Fatal(err)
								}
							}
							err := tx.Commit()
							if c.failCommit && err == nil {
								t.Fatal("Commit returned nil, want the server's error")
							}
							if !c.failCommit && err != nil {
								t.Fatalf("Commit = %v, want nil", err)
							}
							assertTxEnded(t, tx, f, label, rollbackAfter)
							if rollbackAfter {
								// the deferred Rollback idiom: safe, ErrTxDone
								if err := tx.Rollback(); !errors.Is(err, ErrTxDone) {
									t.Fatalf("Rollback after Commit = %v, want ErrTxDone", err)
								}
							}
							if st := p.Stats(); st.InUse != 0 {
								t.Fatalf("round %d: the slot was not released: %+v", i, st)
							}
						}
						// MaxOpen 1: still usable, and the conn is reused only when it was idle.
						beginBounded(t, p, label+" (after the loop)")
						want := 1
						if !c.reused {
							want = rounds + 1
						}
						if got := f.sessionCount(); got != want {
							t.Errorf("sessions = %d, want %d (reused=%v)", got, want, c.reused)
						}
					})
				}
			}
		}
	}
}

// A Rollback that fails ends the Tx too: the conn is discarded (the driver does not
// know its state), the slot is released once, and everything after returns ErrTxDone.
func TestTxFailedRollbackEndsTheTx(t *testing.T) {
	for _, loop := range []bool{false, true} {
		for _, mode := range []string{"close", "err-open", "err-idle"} {
			name := "direct"
			if loop {
				name = "loop"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				f := startEndFakePG(t, "ok", mode)
				p := openEndPool(t, f.addr, loop)
				for i := 0; i < 5; i++ {
					tx := beginBounded(t, p, mode)
					if err := tx.Savepoint(context.Background(), "sp"); err != nil {
						t.Fatal(err)
					}
					if err := tx.Rollback(); err == nil {
						t.Fatal("Rollback returned nil, want the failure")
					}
					assertTxEnded(t, tx, f, mode, true)
					if st := p.Stats(); st.InUse != 0 {
						t.Fatalf("round %d: slot not released: %+v", i, st)
					}
				}
				want := 6
				if mode == "err-idle" {
					want = 1 // ReadyForQuery I: the driver knows the session is idle
				}
				beginBounded(t, p, mode+" (after the loop)")
				if got := f.sessionCount(); got != want {
					t.Errorf("sessions = %d, want %d", got, want)
				}
			})
		}
	}
}

// The release is exactly once even when several goroutines finish the same Tx.
func TestTxFailedCommitReleasesOnceUnderRace(t *testing.T) {
	f := startEndFakePG(t, "err-idle", "ok")
	p := openEndPool(t, f.addr, false)
	for i := 0; i < 50; i++ {
		tx := beginBounded(t, p, "race")
		var wg sync.WaitGroup
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				if k%2 == 0 {
					_ = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}(k)
		}
		wg.Wait()
		if st := p.Stats(); st.InUse != 0 || st.Open > 1 {
			t.Fatalf("iteration %d: %+v", i, st)
		}
	}
	// A double release would let a second Begin run concurrently on MaxOpen 1.
	a := beginBounded(t, p, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.BeginTx(ctx, nil); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("second BeginTx on MaxOpen 1 while the first is open = %v, want a deadline error", err)
	}
	_ = a.Rollback()
}
