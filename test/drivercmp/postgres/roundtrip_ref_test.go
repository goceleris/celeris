package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/lib/pq"

	_ "github.com/goceleris/celeris/driver/postgres"
)

// TestRoundTripAgainstReferenceDrivers reads the common column types through
// celeris-postgres, pgx/v5 (its database/sql adapter) and lib/pq, in the
// simple protocol (no arguments) and the extended protocol (an argument), and
// requires the three to hand the application the same value. It is the
// reference half of test/conformance/postgres/roundtrip_types_test.go, whose
// expectations are literals: here the literals are not trusted, the other two
// drivers are the oracle. The module is separate so pgx and lib/pq never
// reach consumers of celeris.
//
// Each value is first reduced to a canonical string by the kind of column
// (the drivers legitimately differ in the Go type they return: numeric is
// string for pgx and []byte for lib/pq, a timestamptz comes back in the
// session's zone for pgx), then compared.
//
// Cells where celeris is wrong today (the ids are those of the conformance
// package's failures list) run only with -tags pgrtbugs.
func TestRoundTripAgainstReferenceDrivers(t *testing.T) {
	dsnStr := strings.TrimSpace(os.Getenv(envDSN))
	if dsnStr == "" {
		t.Skipf("skipping: %s not set", envDSN)
	}
	open := func(name string) *sql.DB {
		db, err := sql.Open(name, dsnStr)
		if err != nil {
			t.Fatalf("sql.Open(%s): %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	cel, pgx, pq := open("celeris-postgres"), open("pgx"), open("postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := cel.PingContext(ctx); err != nil {
		t.Skipf("skipping: ping: %v", err)
	}
	enum := fmt.Sprintf("celeris_rt_mood_%d", time.Now().UnixNano()%1e9)
	if _, err := cel.ExecContext(ctx, "CREATE TYPE "+enum+" AS ENUM ('sad','ok','happy')"); err != nil {
		t.Fatalf("CREATE TYPE: %v", err)
	}
	t.Cleanup(func() { _, _ = cel.ExecContext(context.Background(), "DROP TYPE IF EXISTS "+enum) })

	const (
		bNoCodec = "B3 no codec: celeris returns binary wire bytes on the extended protocol"
		bBC      = "B5 celeris cannot parse a BC date in a text result"
	)
	type refCase struct {
		name string
		expr string // SELECT expression
		kind string // canonicalisation: text, bool, int, f32, f64, instant, wall, date, tod, bytes
		bugX string // known celeris bug on the extended protocol
		bugS string // known celeris bug on the simple protocol
	}
	cases := []refCase{
		{name: "bool_true", expr: "true", kind: "bool"},
		{name: "bool_false", expr: "false", kind: "bool"},
		{name: "int2", expr: "(-32768)::int2", kind: "int"},
		{name: "int4", expr: "2147483647::int4", kind: "int"},
		{name: "int8", expr: "(-9223372036854775808)::int8", kind: "int"},
		{name: "float4", expr: "1.5::float4", kind: "f32"},
		{name: "float4_tenth", expr: "0.1::float4", kind: "f32"},
		{name: "float8", expr: "3.141592653589793::float8", kind: "f64"},
		{name: "float8_nan", expr: "'NaN'::float8", kind: "f64"},
		{name: "float8_neginf", expr: "'-Infinity'::float8", kind: "f64"},
		{name: "numeric", expr: "(-123456789.123456789)::numeric", kind: "text"},
		{name: "numeric_scale", expr: "12.50::numeric", kind: "text"},
		{name: "numeric_big", expr: "123456789012345678901234567890.123456789::numeric", kind: "text"},
		{name: "numeric_nan", expr: "'NaN'::numeric", kind: "text"},
		{name: "text", expr: "'héllo 世界 🚀'::text", kind: "text"},
		{name: "text_empty", expr: "''::text", kind: "text"},
		{name: "varchar", expr: "'x'::varchar", kind: "text"},
		{name: "char_padded", expr: "'ab'::char(4)", kind: "text"},
		{name: "bytea", expr: "'\\x00ff10'::bytea", kind: "bytes"},
		{name: "bytea_empty", expr: "''::bytea", kind: "bytes"},
		// jsonb is not here yet: its version-byte fix is in flight.
		{name: "json", expr: `'{"a": [1, 2, {"b": null}], "c": "d"}'::json`, kind: "text"},
		{name: "uuid", expr: "'550e8400-e29b-41d4-a716-446655440000'::uuid", kind: "text"},
		{name: "date", expr: "'2025-01-15'::date", kind: "date"},
		{name: "date_bc", expr: "'0044-03-15 BC'::date", kind: "date", bugS: bBC},
		{name: "time", expr: "'12:34:56.789'::time", kind: "tod", bugX: bNoCodec},
		{name: "timestamp", expr: "'2025-01-15 12:34:56.789123'::timestamp", kind: "wall"},
		{name: "timestamptz", expr: "'2025-01-15 12:34:56.789123+00'::timestamptz", kind: "instant"},
		{name: "timestamptz_offset", expr: "'2025-01-15 12:34:56+05:30'::timestamptz", kind: "instant"},
		{name: "interval", expr: "'1 year 2 mons 3 days 04:05:06.789'::interval", kind: "text", bugX: bNoCodec},
		{name: "inet", expr: "'192.168.1.5/24'::inet", kind: "text", bugX: bNoCodec},
		{name: "cidr", expr: "'10.0.0.0/8'::cidr", kind: "text", bugX: bNoCodec},
		{name: "enum", expr: "'happy'::" + enum, kind: "text"},
		{name: "null_int4", expr: "NULL::int4", kind: "null"},
		{name: "null_text", expr: "NULL::text", kind: "null"},
		{name: "null_timestamptz", expr: "NULL::timestamptz", kind: "null"},
		{name: "null_uuid", expr: "NULL::uuid", kind: "null"},
		{name: "null_numeric", expr: "NULL::numeric", kind: "null"},
	}

	canon := func(kind string, v any) (string, error) {
		if v == nil {
			return "NULL", nil
		}
		asString := func() (string, bool) {
			switch x := v.(type) {
			case string:
				return x, true
			case []byte:
				return string(x), true
			}
			return "", false
		}
		switch kind {
		case "null":
			return "", fmt.Errorf("got %T(%v), want NULL", v, v)
		case "text":
			if s, ok := asString(); ok {
				return "text:" + s, nil
			}
		case "bytes":
			if b, ok := v.([]byte); ok {
				return fmt.Sprintf("bytes:%x", b), nil
			}
		case "bool":
			if b, ok := v.(bool); ok {
				return "bool:" + strconv.FormatBool(b), nil
			}
		case "int":
			if n, ok := v.(int64); ok {
				return "int:" + strconv.FormatInt(n, 10), nil
			}
		case "f32", "f64":
			if f, ok := v.(float64); ok {
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return "float:" + strconv.FormatFloat(f, 'g', -1, 64), nil
				}
				if kind == "f32" { // lib/pq parses the text as float64, pgx widens the float32
					return "float:" + strconv.FormatFloat(float64(float32(f)), 'g', -1, 32), nil
				}
				return "float:" + strconv.FormatFloat(f, 'g', -1, 64), nil
			}
		case "instant":
			if tm, ok := v.(time.Time); ok {
				return "instant:" + tm.UTC().Format(time.RFC3339Nano), nil
			}
		case "wall":
			if tm, ok := v.(time.Time); ok { // timestamp has no zone: the wall clock
				return "wall:" + tm.Format("2006-01-02T15:04:05.999999999"), nil
			}
		case "date":
			if tm, ok := v.(time.Time); ok {
				return "date:" + tm.Format("2006-01-02"), nil
			}
		case "tod":
			if tm, ok := v.(time.Time); ok { // lib/pq returns a time.Time on 0000-01-01
				return "tod:" + tm.Format("15:04:05.999999"), nil
			}
			if s, ok := asString(); ok {
				return "tod:" + s, nil
			}
		}
		return "", fmt.Errorf("unexpected %T(%v) for kind %s", v, v, kind)
	}

	read := func(db *sql.DB, q string, args ...any) (any, error) {
		var v any
		err := db.QueryRowContext(ctx, q, args...).Scan(&v)
		return v, err
	}
	for _, c := range cases {
		for _, proto := range []string{"simple", "extended"} {
			bug := c.bugS
			q := "SELECT " + c.expr
			var args []any
			if proto == "extended" {
				bug = c.bugX
				q += " WHERE $1::int = 1"
				args = []any{1}
			}
			if bug != "" && !rtKnownBugs {
				continue
			}
			t.Run(c.name+"/"+proto, func(t *testing.T) {
				if bug != "" {
					t.Logf("known bug: %s", bug)
				}
				got := map[string]string{}
				for name, db := range map[string]*sql.DB{"celeris": cel, "pgx": pgx, "pq": pq} {
					v, err := read(db, q, args...)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					s, err := canon(c.kind, v)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					got[name] = s
				}
				if got["celeris"] != got["pgx"] || got["celeris"] != got["pq"] {
					t.Fatalf("drivers disagree on %s: celeris=%q pgx=%q lib/pq=%q", c.expr, got["celeris"], got["pgx"], got["pq"])
				}
			})
		}
	}
}
