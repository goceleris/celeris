//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// The extended protocol asks the server for result columns in a format. A
// column whose type the driver has no binary codec for must come back in the
// text format, so the application gets the PostgreSQL text form: as a string
// or []byte when it scans into one, and never the binary wire bytes. These
// cells read such types back through every extended-protocol path (auto-cached
// statement, auto_cache_statements=false, an explicit Prepare) on both the
// database/sql adapter and the native Pool, into any, string and []byte. They
// run in the default conformance build and skip cleanly without a server
// (CELERIS_PG_DSN unset).
//
// The matching inventory with every classified cell is in the round-trip
// matrix (roundtrip_types_test.go, `pgrtbugs`).

type rfCase struct {
	name string
	typ  string // column type; %ENUM% and %PAIR% are replaced by per-run types
	lit  string // SQL literal for the value
	text string // the text form the server prints for it
	cast bool   // lit carries its own cast; otherwise ::typ is appended
}

func rfCases() []rfCase {
	return []rfCase{
		{"time", "time", "'12:34:56.789'", "12:34:56.789", false},
		{"timetz", "timetz", "'12:34:56.789+05:30'", "12:34:56.789+05:30", false},
		{"interval", "interval", "'1 year 2 mons 3 days 04:05:06.789'", "1 year 2 mons 3 days 04:05:06.789", false},
		{"interval_negative", "interval", "'-1 day'", "-1 days", false},
		{"inet_v4_mask", "inet", "'192.168.1.5/24'", "192.168.1.5/24", false},
		{"inet_v6", "inet", "'::1'", "::1", false},
		{"cidr_v4", "cidr", "'10.0.0.0/8'", "10.0.0.0/8", false},
		{"cidr_v6", "cidr", "'2001:db8::/32'", "2001:db8::/32", false},
		{"macaddr", "macaddr", "'08:00:2b:01:02:03'", "08:00:2b:01:02:03", false},
		{"oid", "oid", "4294967295", "4294967295", false},
		{"numeric_array", "numeric[]", "'{1.5,2.50}'", "{1.5,2.50}", false},
		{"date_array", "date[]", "'{2025-01-15}'", "{2025-01-15}", false},
		{"timestamptz_array", "timestamptz[]", `'{"2025-01-15 12:00:00+00"}'`, `{"2025-01-15 12:00:00+00"}`, false},
		{"enum_array", "%ENUM%[]", "'{happy,sad}'", "{happy,sad}", false},
		// Types the driver has never heard of: the class, not a list.
		{"int4range", "int4range", "'[1,5)'", "[1,5)", false},
		{"point", "point", "'(1,2)'", "(1,2)", false},
		{"tsvector", "tsvector", "'a:1 b:2'::tsvector", "'a':1 'b':2", true},
		{"composite", "%PAIR%", "ROW(1,'x')", "(1,x)", false},
		{"bit", "bit(5)", "B'10110'", "10110", false},
		{"money", "money", "'12.50'::money", "", true}, // text depends on lc_monetary: only checked for not-binary below
		{"xml", "xml", "'<a>1</a>'", "<a>1</a>", false},
		{"int4multirange", "int4multirange", "'{[1,3),[5,7)}'", "{[1,3),[5,7)}", false},
	}
}

type rfRow interface{ Scan(dest ...any) error }

type rfVariant struct {
	name  string
	query func(ctx context.Context, q string, args ...any) rfRow
}

type rfErrRow struct{ err error }

func (r rfErrRow) Scan(...any) error { return r.err }

type rfRowsRow struct {
	rows *sql.Rows
	stmt *sql.Stmt
}

func (r *rfRowsRow) Scan(dest ...any) error {
	defer func() {
		_ = r.rows.Close()
		_ = r.stmt.Close()
	}()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

func rfVariants(t *testing.T) []rfVariant {
	t.Helper()
	dsn := dsnFromEnv(t)
	nocache := dsn
	if strings.Contains(nocache, "?") {
		nocache += "&auto_cache_statements=false"
	} else {
		nocache += "?auto_cache_statements=false"
	}
	db := openDB(t)
	dbNC, err := sql.Open(driverName, nocache)
	if err != nil {
		t.Fatalf("sql.Open (nocache): %v", err)
	}
	t.Cleanup(func() { _ = dbNC.Close() })
	pool := openPool(t)
	poolNC, err := postgres.Open(nocache, postgres.WithMaxOpen(4))
	if err != nil {
		t.Fatalf("postgres.Open (nocache): %v", err)
	}
	t.Cleanup(func() { _ = poolNC.Close() })

	sqlQ := func(d *sql.DB) func(context.Context, string, ...any) rfRow {
		return func(ctx context.Context, q string, args ...any) rfRow { return d.QueryRowContext(ctx, q, args...) }
	}
	poolQ := func(p *postgres.Pool) func(context.Context, string, ...any) rfRow {
		return func(ctx context.Context, q string, args ...any) rfRow { return p.QueryRow(ctx, q, args...) }
	}
	prep := func(ctx context.Context, q string, args ...any) rfRow {
		stmt, err := db.PrepareContext(ctx, q)
		if err != nil {
			return rfErrRow{err}
		}
		rows, err := stmt.QueryContext(ctx, args...)
		if err != nil {
			_ = stmt.Close()
			return rfErrRow{err}
		}
		return &rfRowsRow{rows: rows, stmt: stmt}
	}
	return []rfVariant{
		{"sql-ext", sqlQ(db)},
		{"sql-nocache", sqlQ(dbNC)},
		{"sql-prep", prep},
		{"pool-ext", poolQ(pool)},
		{"pool-nocache", poolQ(poolNC)},
	}
}

func rfCreateType(t *testing.T, db *sql.DB, name, def string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE TYPE "+name+" AS "+def); err != nil {
		t.Fatalf("CREATE TYPE %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP TYPE IF EXISTS "+name+" CASCADE")
	})
}

// rfText renders a value read back as `any` when it is text (string or []byte).
func rfText(got any) (string, bool) {
	switch v := got.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// TestResultFormatsTextForTypesWithoutCodec reads types the driver has no
// binary codec for through every extended path. Each query runs three times:
// the first one prepares and caches, the next two hit the cache.
func TestResultFormatsTextForTypesWithoutCodec(t *testing.T) {
	db := openDB(t)
	variants := rfVariants(t)
	enum := uniqueTableName(t, "rfmood")
	rfCreateType(t, db, enum, "ENUM ('sad','ok','happy')")
	pair := uniqueTableName(t, "rfpair")
	rfCreateType(t, db, pair, "(a int, b text)")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, c := range rfCases() {
		typ := strings.NewReplacer("%ENUM%", enum, "%PAIR%", pair).Replace(c.typ)
		lit := c.lit
		if !c.cast {
			lit += "::" + typ
		}
		q := "SELECT $1::int, " + lit
		for _, v := range variants {
			for _, dest := range []string{"any", "string", "bytes"} {
				t.Run(fmt.Sprintf("%s/%s/%s", c.name, v.name, dest), func(t *testing.T) {
					for round := 0; round < 3; round++ {
						var k int
						var got string
						var err error
						switch dest {
						case "any":
							var a any
							err = v.query(ctx, q, round).Scan(&k, &a)
							if err == nil {
								s, ok := rfText(a)
								if !ok {
									t.Fatalf("round %d: got %T(%v), want the text form %q", round, a, a, c.text)
								}
								got = s
							}
						case "string":
							err = v.query(ctx, q, round).Scan(&k, &got)
						case "bytes":
							var b []byte
							err = v.query(ctx, q, round).Scan(&k, &b)
							got = string(b)
						}
						if err != nil {
							t.Fatalf("round %d: %v", round, err)
						}
						if k != round {
							t.Fatalf("round %d: first column = %d", round, k)
						}
						if c.text != "" && got != c.text {
							t.Fatalf("round %d: got %q, want the text form %q", round, got, c.text)
						}
						if c.text == "" && (got == "" || strings.ContainsRune(got, 0) || got[0] < 0x20) {
							t.Fatalf("round %d: got %q, which does not look like the text form", round, got)
						}
					}
				})
			}
		}
	}
}

// TestResultFormatsNullOfTypeWithoutCodec checks a NULL of such a type reads
// back as nil/NULL on the same paths.
func TestResultFormatsNullOfTypeWithoutCodec(t *testing.T) {
	variants := rfVariants(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, typ := range []string{"interval", "inet", "numeric[]", "int4range"} {
		for _, v := range variants {
			t.Run(typ+"/"+v.name, func(t *testing.T) {
				for round := 0; round < 2; round++ {
					var k int
					var a any
					if err := v.query(ctx, "SELECT $1::int, NULL::"+typ, round).Scan(&k, &a); err != nil {
						t.Fatalf("Scan: %v", err)
					}
					if a != nil {
						t.Fatalf("NULL read back as %T(%v)", a, a)
					}
					var ns sql.NullString
					if err := v.query(ctx, "SELECT $1::int, NULL::"+typ, round).Scan(&k, &ns); err != nil {
						t.Fatalf("Scan(NullString): %v", err)
					}
					if ns.Valid {
						t.Fatalf("NULL scanned as valid %q", ns.String)
					}
				}
			})
		}
	}
}

// TestResultFormatsMixedRow reads one row mixing columns the driver has a
// codec for with columns it has not: the typed values must keep their Go types
// while the others arrive as text.
func TestResultFormatsMixedRow(t *testing.T) {
	variants := rfVariants(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const q = "SELECT $1::int, 'hello'::text, '1 day'::interval, 42::int8, '10.1.2.3/32'::inet, 1.5::float8, '2025-01-15 12:00:00+00'::timestamptz, true"
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				var (
					k        int
					s        string
					iv       any
					n        int64
					ip       string
					f        float64
					ts       time.Time
					b        bool
					wantTime = time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
				)
				if err := v.query(ctx, q, round).Scan(&k, &s, &iv, &n, &ip, &f, &ts, &b); err != nil {
					t.Fatalf("round %d: Scan: %v", round, err)
				}
				ivt, ok := rfText(iv)
				if k != round || s != "hello" || !ok || ivt != "1 day" || n != 42 || ip != "10.1.2.3" || f != 1.5 || !ts.Equal(wantTime) || !b {
					t.Fatalf("round %d: got %d %q %T(%v) %d %q %v %v %v", round, k, s, iv, iv, n, ip, f, ts, b)
				}
			}
		})
	}
}

// TestResultFormatsMultiDimArray: a multi-dimensional array must never be
// returned flattened. Either the value carries its dimensions (the text
// literal) or the read fails with an error that says so.
func TestResultFormatsMultiDimArray(t *testing.T) {
	variants := rfVariants(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cases := []struct{ name, lit, text string }{
		{"int4_2d", "'{{1,2},{3,4}}'::int4[]", "{{1,2},{3,4}}"},
		{"text_2d", "'{{a,b},{c,d}}'::text[]", "{{a,b},{c,d}}"},
		{"int4_3d", "'{{{1,2},{3,4}},{{5,6},{7,8}}}'::int4[]", "{{{1,2},{3,4}},{{5,6},{7,8}}}"},
		{"int4_lower_bound", "'[0:2]={1,2,3}'::int4[]", "[0:2]={1,2,3}"},
		{"int4_1d", "'{1,2,3}'::int4[]", "{1,2,3}"},
	}
	for _, c := range cases {
		for _, v := range variants {
			t.Run(c.name+"/"+v.name, func(t *testing.T) {
				for round := 0; round < 3; round++ {
					var k int
					var a any
					err := v.query(ctx, "SELECT $1::int, "+c.lit, round).Scan(&k, &a)
					if err != nil {
						if c.name == "int4_1d" {
							t.Fatalf("round %d: %v", round, err)
						}
						if !strings.Contains(err.Error(), "multi-dimensional") && !strings.Contains(err.Error(), "lower bound") {
							t.Fatalf("round %d: error does not say why: %v", round, err)
						}
						continue
					}
					if s, ok := rfText(a); ok {
						if s != c.text {
							t.Fatalf("round %d: got %q, want %q", round, s, c.text)
						}
						continue
					}
					if c.name != "int4_1d" {
						t.Fatalf("round %d: got %T(%v) without error: the dimensions are lost", round, a, a)
					}
				}
			})
		}
	}
}
