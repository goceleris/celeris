//go:build postgres

package postgres_test

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestJSONFamilyBinaryMatchesServerText is a differential fuzz of the codecs
// this branch registers or changes (jsonpath, json[], jsonb[], jsonpath[], and
// the version byte of jsonb): for random values the binary decode (cached and
// unnamed extended paths) must equal the server's own ::text, for a scalar, an
// array element, a NULL element and an empty array. It changes only cells that
// were raw bytes (version byte, array header) in v1.5.11.
func TestJSONFamilyBinaryMatchesServerText(t *testing.T) {
	dsn := dsnFromEnv(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	open := func(d string) *postgres.Pool {
		p, err := postgres.Open(d, postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	pools := map[string]*postgres.Pool{"cached": open(dsn), "unnamed": open(dsn + sep + "auto_cache_statements=false")}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	rng := rand.New(rand.NewSource(20261124))
	type kind struct{ name, typ, arr string }
	var cases []struct {
		k kind
		v string
	}
	for i := 0; i < 250; i++ {
		cases = append(cases,
			struct {
				k kind
				v string
			}{kind{"json", "json", "json[]"}, randJSON(rng, 3)},
			struct {
				k kind
				v string
			}{kind{"jsonb", "jsonb", "jsonb[]"}, randJSON(rng, 3)},
			struct {
				k kind
				v string
			}{kind{"jsonpath", "jsonpath", "jsonpath[]"}, randJSONPath(rng)})
	}
	for mode, p := range pools {
		for i, c := range cases {
			// The value is an argument, so a pool keeps one statement per type (a
			// statement per value would run into the statement cache's eviction).
			base := fmt.Sprintf("SELECT v, v::text, ARRAY[v, NULL]::%s, ARRAY[]::%s FROM (SELECT $2::%s AS v) t WHERE $1::int = 1", c.k.arr, c.k.arr, c.k.typ)
			var s, srv string
			var arr, empty any
			if err := p.QueryRow(ctx, base, 1, c.v).Scan(&s, &srv, &arr, &empty); err != nil {
				t.Fatalf("%s #%d %s %q: %v", mode, i, c.k.name, c.v, err)
			}
			if s != srv {
				t.Fatalf("%s #%d %s %q: scalar into *string %q, server text %q", mode, i, c.k.name, c.v, s, srv)
			}
			elems, ok := arr.([]driver.Value)
			if !ok || len(elems) != 2 || anyText(elems[0]) != srv || elems[1] != nil {
				t.Fatalf("%s #%d %s %q: array %#v, want [%q nil]", mode, i, c.k.name, c.v, arr, srv)
			}
			if e, ok := empty.([]driver.Value); !ok || len(e) != 0 {
				t.Fatalf("%s #%d %s: empty array %#v", mode, i, c.k.name, empty)
			}
		}
	}
}

func randJSON(r *rand.Rand, depth int) string {
	var gen func(d int) string
	str := func() string {
		alphabet := []string{"a", "b", "key", "é", "日本", "\\n", "\\\"", "\\\\", " ", "x y", "\\u0041", "'", "0", "true"}
		var b strings.Builder
		for i, n := 0, r.Intn(4); i < n; i++ {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	gen = func(d int) string {
		k := r.Intn(8)
		if d <= 0 && k > 5 {
			k = r.Intn(6)
		}
		switch k {
		case 0:
			return "null"
		case 1:
			return fmt.Sprint(r.Intn(1000) - 500)
		case 2:
			return fmt.Sprintf("%g", r.NormFloat64()*1e3)
		case 3:
			return []string{"true", "false"}[r.Intn(2)]
		case 4, 5:
			return `"` + str() + `"`
		case 6:
			var parts []string
			for i, n := 0, r.Intn(4); i < n; i++ {
				parts = append(parts, gen(d-1))
			}
			return "[" + strings.Join(parts, ", ") + "]"
		default:
			var parts []string
			for i, n := 0, r.Intn(4); i < n; i++ {
				parts = append(parts, `"`+str()+`": `+gen(d-1))
			}
			return "{" + strings.Join(parts, ", ") + "}"
		}
	}
	return gen(depth)
}

func randJSONPath(r *rand.Rand) string {
	parts := []string{"$", "$.a", "$.a.b", "$.a[*]", "$.a[0]", `$."k y"`, "$.a[*] ? (@ > 5)", "$.a ? (@.b == \"x\")",
		"$[*].id", "$.a.size()", "strict $.a", "lax $.a.b[1 to 3]", "$.a ? (@ like_regex \"^x\")", "$.* ? (@ >= 1 && @ < 9)", "$.a.double()"}
	return parts[r.Intn(len(parts))]
}
