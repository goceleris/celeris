//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A NULL cell scanned through the native postgres.Pool must never leave the
// destination as it was. A variable reused across rows used to keep the
// previous row's value for a NULL column: *float32, *float64, *time.Time, *any
// and a pointer-to-pointer were left unchanged with no error (the driver.Value
// path, scanValue, left every destination unchanged). The contract: the
// destination is set to its zero value (nil for *any, *[]byte,
// *json.RawMessage and a pointer-to-pointer; Valid=false for the sql.Null*
// types), or Scan returns an error; it is never left unchanged. Runs on every
// supported server version (CELERIS_PG_DSN) and skips cleanly without one.

type nsDest struct {
	name string
	mk   func() (ptr any, isNull func() bool, show func() string)
}

func nsd[T any](name string, sentinel T, isNull func(T) bool) nsDest {
	return nsDest{name, func() (any, func() bool, func() string) {
		v := sentinel
		return &v, func() bool { return isNull(v) }, func() string { return fmt.Sprintf("%#v", v) }
	}}
}

func nsdPtr[T any](name string, sentinel T) nsDest {
	return nsDest{name, func() (any, func() bool, func() string) {
		v := new(T)
		*v = sentinel
		return &v, func() bool { return v == nil }, func() string {
			if v == nil {
				return "nil"
			}
			return fmt.Sprintf("&%#v", *v)
		}
	}}
}

func nsZero[T comparable](v T) bool { var z T; return v == z }

func nsDests() []nsDest {
	ts := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	var anyS any = "STALE"
	return []nsDest{
		nsd("*string", "STALE", nsZero[string]),
		nsd("*[]byte", []byte("STALE"), func(v []byte) bool { return v == nil }),
		nsd("*json.RawMessage", json.RawMessage("STALE"), func(v json.RawMessage) bool { return v == nil }),
		nsd("*int", 7, nsZero[int]),
		nsd("*int16", int16(7), nsZero[int16]),
		nsd("*int32", int32(7), nsZero[int32]),
		nsd("*int64", int64(7), nsZero[int64]),
		nsd("*uint32", uint32(7), nsZero[uint32]),
		nsd("*float32", float32(7), nsZero[float32]),
		nsd("*float64", float64(7), nsZero[float64]),
		nsd("*bool", true, nsZero[bool]),
		nsd("*time.Time", ts, func(v time.Time) bool { return v.IsZero() }),
		nsd("*any", anyS, func(v any) bool { return v == nil }),
		nsd("*sql.NullString", sql.NullString{String: "STALE", Valid: true}, func(v sql.NullString) bool { return v == sql.NullString{} }),
		nsd("*sql.NullInt64", sql.NullInt64{Int64: 7, Valid: true}, func(v sql.NullInt64) bool { return v == sql.NullInt64{} }),
		nsd("*sql.NullFloat64", sql.NullFloat64{Float64: 7, Valid: true}, func(v sql.NullFloat64) bool { return v == sql.NullFloat64{} }),
		nsd("*sql.NullBool", sql.NullBool{Bool: true, Valid: true}, func(v sql.NullBool) bool { return v == sql.NullBool{} }),
		nsd("*sql.NullTime", sql.NullTime{Time: ts, Valid: true}, func(v sql.NullTime) bool { return !v.Valid }),
		nsdPtr("**string", "STALE"),
		nsdPtr("**int64", int64(7)),
		nsdPtr("**float64", float64(7)),
		nsdPtr("**bool", true),
		nsdPtr("**time.Time", ts),
		nsdPtr("**[]byte", []byte("STALE")),
	}
}

var nsTypes = []string{
	"int2", "int4", "int8", "float4", "float8", "numeric", "text", "varchar", "bool", "bytea", "uuid",
	"date", "timestamp", "timestamptz", "json", "jsonb", "int4[]", "text[]", "interval", "inet",
}

// Every NULL type into every destination, pre-filled with a sentinel, through
// the three native entry points (simple-protocol QueryRow, extended-protocol
// QueryRow, and Rows.Scan over a 300-row result with one variable reused for
// every row). They all reach the raw *pgRows scan path; the driver.Value path
// (scanValue) is covered by the unit test in driver/postgres, since no query
// here reaches it against a real server.
func TestPoolScanNullNeverLeavesStaleValue(t *testing.T) {
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, typ := range nsTypes {
		for _, d := range nsDests() {
			for _, path := range []string{"queryrow", "queryrow-args", "rows"} {
				ptr, isNull, show := d.mk()
				var err error
				switch path {
				case "queryrow":
					err = pool.QueryRow(ctx, "SELECT NULL::"+typ).Scan(ptr)
				case "queryrow-args":
					err = pool.QueryRow(ctx, "SELECT NULL::"+typ+" WHERE $1::int = 1", 1).Scan(ptr)
				case "rows":
					rows, qerr := pool.QueryContext(ctx, "SELECT NULL::"+typ+" FROM generate_series(1, 300)")
					if qerr != nil {
						t.Fatalf("%s: %v", typ, qerr)
					}
					for rows.Next() {
						if err = rows.Scan(ptr); err != nil {
							break
						}
					}
					if err == nil {
						err = rows.Err()
					}
					_ = rows.Close()
				}
				if err != nil {
					t.Errorf("NULL::%s into %s (%s): unexpected error: %v", typ, d.name, path, err)
					continue
				}
				if !isNull() {
					t.Errorf("NULL::%s into %s (%s): destination keeps its previous value %s", typ, d.name, path, show())
				}
			}
		}
	}
}

// The case users hit: one variable scanned across rows, a NULL after a value.
func TestPoolScanNullAfterValueRow(t *testing.T) {
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := pool.QueryContext(ctx, `SELECT * FROM (VALUES
		(1, 1.5::float8, 2.5::float4, 'a'::text, TRUE, TIMESTAMPTZ '2025-01-02 03:04:05+00', 'x'::bytea, 9::int8, '{"k":1}'::jsonb),
		(2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)
	) AS v(id, f8, f4, s, b, ts, by, n8, j) ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		id int
		f8 float64
		f4 float32
		s  string
		b  bool
		ts time.Time
		by []byte
		n8 int64
		j  json.RawMessage
	)
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	if err := rows.Scan(&id, &f8, &f4, &s, &b, &ts, &by, &n8, &j); err != nil {
		t.Fatalf("scan row 1: %v", err)
	}
	if f8 != 1.5 || f4 != 2.5 || s != "a" || !b || ts.IsZero() || string(by) != "x" || n8 != 9 || len(j) == 0 {
		t.Fatalf("row 1 decoded wrong: %v %v %q %v %v %q %v %s", f8, f4, s, b, ts, by, n8, j)
	}
	if !rows.Next() {
		t.Fatalf("no second row: %v", rows.Err())
	}
	if err := rows.Scan(&id, &f8, &f4, &s, &b, &ts, &by, &n8, &j); err != nil {
		t.Fatalf("scan row 2: %v", err)
	}
	if id != 2 || f8 != 0 || f4 != 0 || s != "" || b || !ts.IsZero() || by != nil || n8 != 0 || j != nil {
		t.Errorf("row 2 (all NULL) left stale values: id=%d f8=%v f4=%v s=%q b=%v ts=%v by=%v n8=%v j=%s", id, f8, f4, s, b, ts, by, n8, j)
	}

	// *any: a value then NULL.
	var v any
	rows2, err := pool.QueryContext(ctx, `SELECT x FROM (VALUES (1, 42::int8), (2, NULL::int8)) AS t(o, x) ORDER BY o`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows2.Close() }()
	var seen []string
	for rows2.Next() {
		if err := rows2.Scan(&v); err != nil {
			t.Fatalf("scan *any: %v", err)
		}
		seen = append(seen, fmt.Sprintf("%v", v))
	}
	if got := strings.Join(seen, ","); got != "42,<nil>" {
		t.Errorf("*any across a value then NULL = %s, want 42,<nil>", got)
	}

	// **T: allocates for a value, nil for NULL.
	var pf *float64
	rows3, err := pool.QueryContext(ctx, `SELECT x FROM (VALUES (1, 2.5::float8), (2, NULL::float8)) AS t(o, x) ORDER BY o`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows3.Close() }()
	if !rows3.Next() {
		t.Fatal("no row")
	}
	if err := rows3.Scan(&pf); err != nil || pf == nil || *pf != 2.5 {
		t.Fatalf("**float64 <- 2.5: %v err %v", pf, err)
	}
	if !rows3.Next() {
		t.Fatal("no second row")
	}
	if err := rows3.Scan(&pf); err != nil || pf != nil {
		t.Fatalf("**float64 <- NULL: %v err %v, want nil", pf, err)
	}
}

// A destination Scan does not support fails for NULL as it does for a value.
func TestPoolScanNullUnsupportedDest(t *testing.T) {
	pool := openPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, dest := range map[string]any{"*uint64": new(uint64), "*int8": new(int8), "*[]string": new([]string)} {
		for _, q := range []string{"SELECT NULL::int8", "SELECT 1::int8"} {
			err := pool.QueryRow(ctx, q).Scan(dest)
			if err == nil || !strings.Contains(err.Error(), "unsupported scan dest type") {
				t.Errorf("%s into %s: err = %v, want unsupported scan dest type", q, name, err)
			}
		}
	}
}
