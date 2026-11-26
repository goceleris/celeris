//go:build postgres && pgbouncer

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// PgBouncer in transaction mode WITHOUT prepared-statement support
// (max_prepared_statements=0, pgbouncer-noprep/pgbouncer.ini) shares its server
// connections between client connections and passes a named Parse and Bind
// through without tracking or renaming it. The property tested here is that no
// call ever runs a statement its own connection did not Parse with that exact
// SQL (celeris#1455): when two clients both named a statement celst_1, a Bind
// of celst_1 ran whichever client's SQL lived on the server connection, and a
// client that only ran SELECTs executed another client's INSERT.
//
// Errors are allowed and counted (26000 "does not exist" when the Parse and the
// Bind of a cached statement land on different server connections, 42P05
// "already exists"); a wrong statement is not: a SELECT-only client changes no
// row and no audit row, every row it gets back carries its own marker, and
// every audit row of a writer is that writer's own.
//
// Each client is a separate process: this test binary run again with
// CELERIS_NOPREP_ROLE set (TestPgBouncerNoPrepChild). It needs
// CELERIS_PGBOUNCER_NOPREP_DSN, a DSN of the transaction-mode database of that
// PgBouncer, and skips without it. It uses only the public API, so the same
// file runs against an older tag.
const (
	envPgBouncerNoPrepDSN = "CELERIS_PGBOUNCER_NOPREP_DSN"
	envNoPrepRole         = "CELERIS_NOPREP_ROLE" // "select" or "write": the child's role
	envNoPrepSpec         = "CELERIS_NOPREP_SPEC" // JSON childSpec
)

type childSpec struct {
	DSN     string `json:"dsn"`
	Table   string `json:"table"`
	ID      string `json:"id"`  // process id: the marker of everything the process runs
	API     string `json:"api"` // "pool" (native), "sql" (database/sql) or "prep" (database/sql, explicit Prepare)
	N       int    `json:"n"`   // iterations per goroutine
	Workers int    `json:"workers"`
	StartAt int64  `json:"start_at"` // unix nano: every process starts its calls then
}

type childResult struct {
	ID       string         `json:"id"`
	Role     string         `json:"role"`
	Calls    int            `json:"calls"`
	OK       int            `json:"ok"`
	Wrong    int            `json:"wrong"` // wrong value, wrong row count or a row of another statement
	Errs     map[string]int `json:"errs"`  // SQLSTATE, or "other", of the calls that failed
	Other    []string       `json:"other"` // first messages of the "other" errors
	Affected map[string]int `json:"affected"`
}

func (r *childResult) fail(err error) {
	var pe *postgres.PGError
	if errors.As(err, &pe) {
		r.Errs[pe.Code]++
		return
	}
	r.Errs["other"]++
	if len(r.Other) < 5 {
		r.Other = append(r.Other, err.Error())
	}
}

func TestPgBouncerNoPrepChild(t *testing.T) {
	role := os.Getenv(envNoPrepRole)
	if role == "" {
		t.Skip("skipping: helper of TestPgBouncerNoPrep, runs only as a child process of it")
	}
	var sp childSpec
	if err := json.Unmarshal([]byte(os.Getenv(envNoPrepSpec)), &sp); err != nil {
		t.Fatalf("spec: %v", err)
	}
	res := &childResult{ID: sp.ID, Role: role, Errs: map[string]int{}, Affected: map[string]int{}}
	var mu sync.Mutex
	merge := func(l *childResult) {
		mu.Lock()
		defer mu.Unlock()
		res.Calls += l.Calls
		res.OK += l.OK
		res.Wrong += l.Wrong
		for k, v := range l.Errs {
			res.Errs[k] += v
		}
		for k, v := range l.Affected {
			res.Affected[k] += v
		}
		for _, o := range l.Other {
			if len(res.Other) < 5 {
				res.Other = append(res.Other, o)
			}
		}
	}

	var (
		pool *postgres.Pool
		db   *sql.DB
		err  error
	)
	if sp.API == "pool" {
		pool, err = postgres.Open(sp.DSN, postgres.WithMaxOpen(sp.Workers))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		defer func() { _ = pool.Close() }()
	} else {
		db, err = sql.Open(postgres.DriverName, sp.DSN)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		db.SetMaxOpenConns(sp.Workers)
		defer func() { _ = db.Close() }()
	}

	// The statements. Every one takes ($1 text, $2 text, $3 int8): a SELECT
	// that runs as one of the writers' statements would be a valid Bind, and
	// would write the SELECT client's values.
	insQ := fmt.Sprintf("INSERT INTO %s(who, a, b) VALUES($1, $2, $3)", sp.Table)
	updQ := fmt.Sprintf("UPDATE %s SET b = b + 1 WHERE who = $1 AND a = $2 AND b >= $3", sp.Table)
	delQ := fmt.Sprintf("DELETE FROM %s WHERE who = $1 AND a = $2 AND b >= $3", sp.Table)
	selQ := fmt.Sprintf("SELECT 'sel-%s'::text, $1::text, $2::text, $3::int8", sp.ID)

	var stmts map[string]*sql.Stmt
	if sp.API == "prep" {
		stmts = map[string]*sql.Stmt{}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for k, q := range map[string]string{"I": insQ, "U": updQ, "D": delQ} {
			// A Prepare can fail (42P05 on an old driver, a pooler that
			// moves the connection): it is counted, and that op runs unprepared.
			st, perr := db.PrepareContext(ctx, q)
			if perr != nil {
				res.Errs["prepare"]++
				continue
			}
			stmts[k] = st
		}
	}

	ready := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < sp.Workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			l := &childResult{Errs: map[string]int{}, Affected: map[string]int{}}
			defer merge(l)
			<-ready
			exec := func(op, q string, args ...any) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				l.Calls++
				var n int64
				var err error
				switch {
				case sp.API == "pool":
					var r postgres.Result
					r, err = pool.ExecContext(ctx, q, args...)
					if err == nil {
						n, _ = r.RowsAffected()
					}
				case sp.API == "prep" && stmts[op] != nil:
					var r sql.Result
					r, err = stmts[op].ExecContext(ctx, args...)
					if err == nil {
						n, _ = r.RowsAffected()
					}
				default:
					var r sql.Result
					r, err = db.ExecContext(ctx, q, args...)
					if err == nil {
						n, _ = r.RowsAffected()
					}
				}
				if err != nil {
					l.fail(err)
					return
				}
				l.OK++
				l.Affected[op] += int(n)
			}
			query := func(i int) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				l.Calls++
				marker := fmt.Sprintf("%s-g%d-%d", sp.ID, g, i)
				var tag, a, b string
				var c int64
				var err error
				got := 0
				if sp.API == "pool" && i%2 == 0 {
					var rows *postgres.Rows
					rows, err = pool.QueryContext(ctx, selQ, sp.ID, marker, int64(i))
					if err == nil {
						for rows.Next() {
							got++
							if err = rows.Scan(&tag, &a, &b, &c); err != nil {
								break
							}
						}
						if err == nil {
							err = rows.Err()
						}
						_ = rows.Close()
					}
				} else if sp.API == "pool" {
					got = 1
					err = pool.QueryRow(ctx, selQ, sp.ID, marker, int64(i)).Scan(&tag, &a, &b, &c)
				} else {
					got = 1
					err = db.QueryRowContext(ctx, selQ, sp.ID, marker, int64(i)).Scan(&tag, &a, &b, &c)
				}
				if errors.Is(err, sql.ErrNoRows) {
					l.Wrong++ // a SELECT that returned no row: another statement ran
					return
				}
				if err != nil {
					l.fail(err)
					return
				}
				if got != 1 || tag != "sel-"+sp.ID || a != sp.ID || b != marker || c != int64(i) {
					l.Wrong++
					return
				}
				l.OK++
			}
			for i := 0; i < sp.N; i++ {
				if role == "select" {
					query(i)
					continue
				}
				who, a := sp.ID, fmt.Sprintf("%s-g%d-%d", sp.ID, g, i)
				exec("I", insQ, who, a, int64(i))
				exec("U", updQ, who, a, int64(0))
				exec("D", delQ, who, a, int64(0))
			}
		}(g)
	}
	if sp.API != "pool" {
		// Dial before the start line so the calls of all processes overlap.
		pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = db.PingContext(pctx)
		cancel()
	}
	if d := time.Until(time.Unix(0, sp.StartAt)); d > 0 {
		time.Sleep(d)
	}
	close(ready)
	wg.Wait()
	for _, st := range stmts {
		_ = st.Close()
	}
	out, _ := json.Marshal(res)
	fmt.Printf("\nNOPREP-RESULT %s\n", out)
}

type noprepProc struct {
	role string
	api  string
	n    int
}

// runNoPrepProcs starts every process at once and returns their results.
func runNoPrepProcs(t *testing.T, dsn, table string, procs []noprepProc) []childResult {
	t.Helper()
	start := time.Now().Add(3 * time.Second).UnixNano()
	results := make([]childResult, len(procs))
	var wg sync.WaitGroup
	for i, p := range procs {
		wg.Add(1)
		go func(i int, p noprepProc) {
			defer wg.Done()
			var rnd [4]byte
			_, _ = rand.Read(rnd[:])
			sp := childSpec{DSN: dsn, Table: table, ID: fmt.Sprintf("%s%d-%s", p.role[:1], i, hex.EncodeToString(rnd[:])), API: p.api, N: p.n, Workers: 4, StartAt: start}
			spec, _ := json.Marshal(sp)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPgBouncerNoPrepChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), envNoPrepRole+"="+p.role, envNoPrepSpec+"="+string(spec))
			out, err := cmd.CombinedOutput()
			var line string
			for _, l := range strings.Split(string(out), "\n") {
				if rest, ok := strings.CutPrefix(l, "NOPREP-RESULT "); ok {
					line = rest
				}
			}
			if line == "" {
				t.Errorf("process %d (%s/%s) gave no result: %v\n%s", i, p.role, p.api, err, out)
				return
			}
			if err := json.Unmarshal([]byte(line), &results[i]); err != nil {
				t.Errorf("process %d: bad result %q: %v", i, line, err)
			}
		}(i, p)
	}
	wg.Wait()
	return results
}

// noprepTable creates a table, an audit table and a trigger that records every
// row any INSERT, UPDATE or DELETE touches. Setup and checks use statements
// without arguments (the simple protocol), which a pooler without prepared
// statements passes through safely.
func noprepTable(t *testing.T, db *sql.DB) (table, audit string) {
	t.Helper()
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	table = "np1455_" + hex.EncodeToString(rnd[:])
	audit = table + "_audit"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, q := range []string{
		fmt.Sprintf("CREATE TABLE %s (id bigserial PRIMARY KEY, who text, a text, b int8)", table),
		fmt.Sprintf("CREATE TABLE %s (op text, who text, a text, b int8)", audit),
		fmt.Sprintf(`CREATE FUNCTION %s_fn() RETURNS trigger LANGUAGE plpgsql AS $f$
			BEGIN
				IF TG_OP = 'DELETE' THEN
					INSERT INTO %s VALUES ('D', OLD.who, OLD.a, OLD.b);
					RETURN OLD;
				END IF;
				INSERT INTO %s VALUES (substr(TG_OP, 1, 1), NEW.who, NEW.a, NEW.b);
				RETURN NEW;
			END $f$`, table, audit, audit),
		fmt.Sprintf("CREATE TRIGGER %s_trg AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION %s_fn()", table, table, table),
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = db.ExecContext(cctx, fmt.Sprintf("DROP TABLE IF EXISTS %s, %s", table, audit))
		_, _ = db.ExecContext(cctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s_fn()", table))
	})
	return table, audit
}

func scalarInt(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// auditCounts returns, per audit who and op, the number of audit rows.
func auditCounts(t *testing.T, db *sql.DB, audit string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT who, op, count(*) FROM %s GROUP BY who, op", audit))
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var who, op string
		var n int
		if err := rows.Scan(&who, &op, &n); err != nil {
			t.Fatal(err)
		}
		out[who+"/"+op] = n
	}
	return out
}

func logNoPrepResults(t *testing.T, rs []childResult) {
	for _, r := range rs {
		keys := make([]string, 0, len(r.Errs))
		for k := range r.Errs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var es []string
		for _, k := range keys {
			es = append(es, fmt.Sprintf("%s=%d", k, r.Errs[k]))
		}
		t.Logf("%-14s %-6s calls=%d ok=%d wrong=%d errors{%s} affected=%v", r.ID, r.Role, r.Calls, r.OK, r.Wrong, strings.Join(es, " "), r.Affected)
	}
}

// checkNoPrep asserts the property over a finished run: errors are allowed
// (maxErrs bounds them for the way-out run, which allows none), wrong
// statements are not.
func checkNoPrep(t *testing.T, db *sql.DB, audit string, rs []childResult, allowErrors bool) {
	t.Helper()
	logNoPrepResults(t, rs)
	want := map[string]int{}
	for _, r := range rs {
		if r.Wrong != 0 {
			t.Errorf("%s (%s): %d calls returned a wrong value or row count", r.ID, r.Role, r.Wrong)
		}
		for code, n := range r.Errs {
			switch {
			case !allowErrors:
				t.Errorf("%s (%s): %d errors %s, want none", r.ID, r.Role, n, code)
			case code != "26000" && code != "42P05":
				t.Errorf("%s (%s): %d errors %s (allowed: 26000, 42P05); first: %v", r.ID, r.Role, n, code, r.Other)
			}
		}
		if r.Role == "select" {
			if r.OK == 0 {
				t.Errorf("%s: the SELECT client got no answer at all (%d calls): the run proves nothing", r.ID, r.Calls)
			}
			for op, n := range r.Affected {
				t.Errorf("%s: a SELECT client reports %d rows affected by %s", r.ID, n, op)
			}
		} else {
			for op, n := range r.Affected {
				want[r.ID+"/"+op] += n
			}
		}
	}
	got := auditCounts(t, db, audit)
	for k, n := range got {
		if want[k] != n {
			t.Errorf("audit rows %s: %d, the writers report %d (a SELECT client, or a statement that was not the caller's, wrote rows)", k, n, want[k])
		}
	}
	for k, n := range want {
		if n != 0 && got[k] != n {
			t.Errorf("audit rows %s: %d, the writers report %d", k, got[k], n)
		}
	}
	if n := scalarInt(t, db, fmt.Sprintf("SELECT count(*) FROM %s WHERE a NOT LIKE who || '-%%'", audit)); n != 0 {
		t.Errorf("%d audit rows whose value is not their writer's", n)
	}
}

func TestPgBouncerNoPrep(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(envPgBouncerNoPrepDSN))
	if dsn == "" {
		t.Skipf("skipping: %s not set (set it to the transaction-mode DSN of a PgBouncer with max_prepared_statements=0)", envPgBouncerNoPrepDSN)
	}
	db, err := sql.Open(postgres.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(2)

	// A run of writers leaves their named statements on the server
	// connections; a SELECT client that runs afterwards, alone, must change
	// nothing: the table, and the audit of every row ever written, stay as the
	// writers left them. (Before: its Bind ran the writers' INSERT.)
	t.Run("select-after-writers", func(t *testing.T) {
		table, audit := noprepTable(t, db)
		w := runNoPrepProcs(t, dsn, table, []noprepProc{{"write", "pool", 40}, {"write", "sql", 40}})
		rowsBefore := scalarInt(t, db, "SELECT count(*) FROM "+table)
		auditBefore := scalarInt(t, db, "SELECT count(*) FROM "+audit)
		s := runNoPrepProcs(t, dsn, table, []noprepProc{{"select", "pool", 150}})
		if n := scalarInt(t, db, "SELECT count(*) FROM "+table); n != rowsBefore {
			t.Errorf("table rows %d -> %d during a SELECT-only run", rowsBefore, n)
		}
		if n := scalarInt(t, db, "SELECT count(*) FROM "+audit); n != auditBefore {
			t.Errorf("audit rows %d -> %d during a SELECT-only run", auditBefore, n)
		}
		checkNoPrep(t, db, audit, append(w, s...), true)
	})

	t.Run("two-processes", func(t *testing.T) {
		table, audit := noprepTable(t, db)
		rs := runNoPrepProcs(t, dsn, table, []noprepProc{{"select", "pool", 150}, {"write", "pool", 60}})
		checkNoPrep(t, db, audit, rs, true)
	})

	t.Run("four-processes", func(t *testing.T) {
		table, audit := noprepTable(t, db)
		rs := runNoPrepProcs(t, dsn, table, []noprepProc{
			{"select", "pool", 150}, {"write", "pool", 60}, {"write", "sql", 60}, {"write", "prep", 60},
		})
		checkNoPrep(t, db, audit, rs, true)
	})

	t.Run("selects-only", func(t *testing.T) {
		table, audit := noprepTable(t, db)
		rs := runNoPrepProcs(t, dsn, table, []noprepProc{{"select", "pool", 150}, {"select", "sql", 150}, {"select", "pool", 150}})
		checkNoPrep(t, db, audit, rs, true)
		if n := scalarInt(t, db, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%d table rows after SELECT-only clients", n)
		}
	})
}

// TestPgBouncerNoPrepWayOut: auto_cache_statements=false is the supported way
// to use the driver behind this PgBouncer (every statement with arguments is an
// unnamed Parse, Bind and Execute in one flight, so no statement lives on the
// server connection between calls; do not use Prepare there): the same
// clients, no error and no wrong statement at all.
func TestPgBouncerNoPrepWayOut(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(envPgBouncerNoPrepDSN))
	if dsn == "" {
		t.Skipf("skipping: %s not set", envPgBouncerNoPrepDSN)
	}
	dsn += pgbouncerParamSep(dsn) + "auto_cache_statements=false"
	db, err := sql.Open(postgres.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(2)
	table, audit := noprepTable(t, db)
	rs := runNoPrepProcs(t, dsn, table, []noprepProc{
		{"select", "pool", 100}, {"select", "sql", 100}, {"write", "pool", 40}, {"write", "sql", 40},
	})
	checkNoPrep(t, db, audit, rs, false)
}
