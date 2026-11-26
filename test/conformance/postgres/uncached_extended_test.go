//go:build postgres

package postgres_test

// Extended statements that are not auto-cached (an INSERT/UPDATE/DELETE ...
// RETURNING with arguments; any query with auto_cache_statements=false) describe
// their portal in the same exchange as the Bind and Execute. What the caller reads
// from them must not depend on when the driver publishes the Describe's columns:
// the column names and types, and the values, are the same as for the same
// statement over the simple protocol, on the native Pool and on database/sql.
// (The statement is cut after its DataRows in driver/postgres/connloss_uncached_test.go.)

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	postgres "github.com/goceleris/celeris/driver/postgres"
)

const uncachedCols = "id, name, flag, ts, price, b"

type uncachedRow struct {
	id    int64
	name  string
	flag  bool
	ts    time.Time
	price string
	b     []byte
}

func (r uncachedRow) String() string {
	return fmt.Sprintf("%d|%s|%v|%s|%s|%x", r.id, r.name, r.flag, r.ts.UTC().Format(time.RFC3339Nano), r.price, r.b)
}

func uncachedWant(id int64, name string) uncachedRow {
	return uncachedRow{id: id, name: name, flag: id%2 == 0, ts: time.Date(2026, 11, 26, 10, 11, 12, 345678000, time.UTC), price: "12.50", b: []byte{0, 0xff, byte(id)}}
}

func TestUncachedExtendedStatements(t *testing.T) {
	dsn := dsnFromEnv(t)
	admin := openDB(t)
	tbl := uniqueTableName(t, "uncached")
	createTable(t, admin, tbl, "id bigint primary key, name text, flag boolean, ts timestamptz, price numeric(10,2), b bytea")

	type stmt struct {
		name  string
		sql   string
		args  func(id int64) []any
		extra string // DSN parameters
		want  func(id int64) uncachedRow
	}
	ins := fmt.Sprintf("INSERT INTO %s (%s) VALUES ($1, $2, $3, $4, $5, $6) RETURNING %s", tbl, uncachedCols, uncachedCols)
	upd := fmt.Sprintf("UPDATE %s SET name = $1 WHERE id = $2 RETURNING %s", tbl, uncachedCols)
	sel := fmt.Sprintf("SELECT %s FROM %s WHERE id = $1", uncachedCols, tbl)
	del := fmt.Sprintf("DELETE FROM %s WHERE id = $1 RETURNING %s", tbl, uncachedCols)
	insArgs := func(id int64) []any {
		w := uncachedWant(id, fmt.Sprintf("n%d", id))
		return []any{w.id, w.name, w.flag, w.ts, w.price, w.b}
	}
	stmts := []stmt{
		{"insert-returning", ins, insArgs, "", func(id int64) uncachedRow { return uncachedWant(id, fmt.Sprintf("n%d", id)) }},
		{"update-returning", upd, func(id int64) []any { return []any{fmt.Sprintf("u%d", id), id} }, "", func(id int64) uncachedRow { return uncachedWant(id, fmt.Sprintf("u%d", id)) }},
		{"select-no-autocache", sel, func(id int64) []any { return []any{id} }, "&auto_cache_statements=false", func(id int64) uncachedRow { return uncachedWant(id, fmt.Sprintf("u%d", id)) }},
		{"delete-returning", del, func(id int64) []any { return []any{id} }, "", func(id int64) uncachedRow { return uncachedWant(id, fmt.Sprintf("u%d", id)) }},
	}
	// The types of the columns, read over the simple protocol (the reference).
	var refTypes []string
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rows, err := admin.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s LIMIT 0", uncachedCols, tbl))
		if err != nil {
			t.Fatal(err)
		}
		cts, _ := rows.ColumnTypes()
		for _, ct := range cts {
			refTypes = append(refTypes, ct.DatabaseTypeName())
		}
		_ = rows.Close()
	}

	for _, viaPool := range []bool{false, true} {
		kind := "sql"
		if viaPool {
			kind = "pool"
		}
		t.Run(kind, func(t *testing.T) {
			id := int64(100)
			if viaPool {
				id = 200
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, st := range stmts {
				st := st
				d := dsn
				if st.extra != "" {
					d += st.extra
				}
				args := st.args(id)
				want := st.want(id)
				var got []uncachedRow
				var cols []string
				var types []string
				if viaPool {
					p, err := openPoolEngine(d, postgres.WithMaxOpen(1))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = p.Close() }()
					rows, err := p.QueryContext(ctx, st.sql, args...)
					if err != nil {
						t.Fatalf("%s: %v", st.name, err)
					}
					cols = rows.Columns()
					for rows.Next() {
						var r uncachedRow
						if err := rows.Scan(&r.id, &r.name, &r.flag, &r.ts, &r.price, &r.b); err != nil {
							t.Fatalf("%s: Scan: %v", st.name, err)
						}
						got = append(got, r)
					}
					if err := rows.Err(); err != nil {
						t.Fatalf("%s: Err: %v", st.name, err)
					}
					if err := rows.Close(); err != nil {
						t.Fatalf("%s: Close: %v", st.name, err)
					}
				} else {
					db, err := openSQLEngine(d)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = db.Close() }()
					db.SetMaxOpenConns(1)
					rows, err := db.QueryContext(ctx, st.sql, args...)
					if err != nil {
						t.Fatalf("%s: %v", st.name, err)
					}
					cols, _ = rows.Columns()
					cts, _ := rows.ColumnTypes()
					for _, ct := range cts {
						types = append(types, ct.DatabaseTypeName())
					}
					for rows.Next() {
						var r uncachedRow
						if err := rows.Scan(&r.id, &r.name, &r.flag, &r.ts, &r.price, &r.b); err != nil {
							t.Fatalf("%s: Scan: %v", st.name, err)
						}
						got = append(got, r)
					}
					if err := rows.Err(); err != nil {
						t.Fatalf("%s: Err: %v", st.name, err)
					}
					if err := rows.Close(); err != nil {
						t.Fatalf("%s: Close: %v", st.name, err)
					}
				}
				if strings.Join(cols, ",") != strings.ReplaceAll(uncachedCols, " ", "") {
					t.Errorf("%s: Columns() = %v, want [%s]", st.name, cols, uncachedCols)
				}
				if !viaPool && strings.Join(types, ",") != strings.Join(refTypes, ",") {
					t.Errorf("%s: column types %v, want the simple protocol's %v", st.name, types, refTypes)
				}
				if len(got) != 1 {
					t.Fatalf("%s: %d rows, want 1", st.name, len(got))
				}
				if got[0].String() != want.String() {
					t.Errorf("%s: row %s, want %s", st.name, got[0], want)
				}
			}
		})
	}
}
