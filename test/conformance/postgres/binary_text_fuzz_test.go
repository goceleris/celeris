//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// Differential fuzz of the binary-to-text codecs (time, timetz, interval, inet,
// cidr, macaddr, macaddr8, oid) against a real server: for random values the
// text the driver produces from the binary result must equal the server's own
// ::text of the same value, on the cached and the unnamed statement (native
// *string and *any) and through database/sql (*string).
//
//	CELERIS_PG_FUZZ_N     cases per type (default 3000; a nightly run uses 500000)
//	CELERIS_PG_FUZZ_SEED  seed of the generator (default 20261124)
//
// Without CELERIS_PG_DSN (and the postgres build tag) the tests skip.

// fuzzBatches alternate: 50 rows is under the driver's streaming threshold (the
// buffered scanRaw path), 250 is over it (the streamed path).
var fuzzBatches = [...]int{50, 250}

func fuzzParams(t *testing.T) (n int, seed uint64) {
	t.Helper()
	n, seed = 3000, 20261124
	if s := os.Getenv("CELERIS_PG_FUZZ_N"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			t.Fatalf("CELERIS_PG_FUZZ_N=%q: want a positive integer", s)
		}
		n = v
	}
	if s := os.Getenv("CELERIS_PG_FUZZ_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("CELERIS_PG_FUZZ_SEED=%q: want an unsigned integer", s)
		}
		seed = v
	}
	return n, seed
}

type fuzzRig struct {
	cached, unnamed *postgres.Pool
	db              *sql.DB
	pgVersionNum    int
	pgVersion       string
}

func newFuzzRig(t *testing.T) *fuzzRig {
	t.Helper()
	dsn := dsnFromEnv(t)
	r := &fuzzRig{
		cached:  openSettingsPool(t, dsn),
		unnamed: openSettingsPool(t, withParams(dsn, "auto_cache_statements=false")),
		db:      openDB(t),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.cached.QueryRow(ctx, "SHOW server_version_num").Scan(&r.pgVersion); err != nil {
		t.Fatalf("SHOW server_version_num: %v", err)
	}
	r.pgVersionNum, _ = strconv.Atoi(r.pgVersion)
	_ = r.cached.QueryRow(ctx, "SHOW server_version").Scan(&r.pgVersion)
	return r
}

// fuzzType runs n generated literals of the type through every route. A literal
// the server rejects (an out-of-range interval) is skipped and counted; more
// than 5% rejected is a generator bug and fails the test.
func fuzzType(t *testing.T, rig *fuzzRig, typ string, seedIdx uint64, gen func(r *rand.Rand, i int) string, edges []string) {
	n, seed := fuzzParams(t)
	r := rand.New(rand.NewPCG(seed, seedIdx))
	lits := make([]string, 0, n+len(edges))
	lits = append(lits, edges...)
	for i := 0; i < n; i++ {
		lits = append(lits, gen(r, i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	checked := map[string]int{}
	rejected := 0
	for start, k := 0, 0; start < len(lits); k++ {
		end := min(start+fuzzBatches[k%len(fuzzBatches)], len(lits))
		batch := lits[start:end]
		start = end
		rej, err := fuzzCheck(ctx, rig, typ, batch, checked)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		rejected += rej
	}
	if rejected*20 > len(lits) {
		t.Fatalf("%s: the server rejected %d of %d generated literals (generator bug)", typ, rejected, len(lits))
	}
	t.Logf("%s on PostgreSQL %s (seed %d): %d literals, %d rejected by the server; compared per route: cached=%d unnamed=%d database/sql=%d",
		typ, rig.pgVersion, seed, len(lits), rejected, checked["cached"], checked["unnamed"], checked["database/sql"])
}

// fuzzCheck compares one batch; on a server error it falls back to one literal
// per query and counts the rejected ones.
func fuzzCheck(ctx context.Context, rig *fuzzRig, typ string, batch []string, checked map[string]int) (rejected int, err error) {
	err = fuzzRun(ctx, rig, typ, batch, checked)
	if !isServerReject(err) {
		return 0, err
	}
	for _, lit := range batch {
		err := fuzzRun(ctx, rig, typ, []string{lit}, checked)
		if err == nil {
			continue
		}
		if !isServerReject(err) {
			return rejected, err
		}
		rejected++
	}
	return rejected, nil
}

// isServerReject is true only for an error the server sent (a literal that is
// out of range for the type). Any other error, a decode failure of the driver
// included, is a failure of the test.
func isServerReject(err error) bool {
	var pe *postgres.PGError
	return errors.As(err, &pe)
}

type fuzzMismatch struct{ msg string }

func (e *fuzzMismatch) Error() string { return e.msg }

// fuzzRun checks one batch on every route; the per-route counts are added to
// checked only when the whole batch passed.
func fuzzRun(ctx context.Context, rig *fuzzRig, typ string, batch []string, checked map[string]int) error {
	local := map[string]int{}
	if err := fuzzRoutes(ctx, rig, typ, batch, local); err != nil {
		return err
	}
	for k, v := range local {
		checked[k] += v
	}
	return nil
}

func fuzzRoutes(ctx context.Context, rig *fuzzRig, typ string, batch []string, checked map[string]int) error {
	// One statement text per type, the literals as one text argument: a
	// statement per batch would churn the statement cache, whose evictions
	// run a DEALLOCATE on the connection in the background (see the PR body).
	q := "SELECT u.v::" + typ + ", " + oracleExpr(typ) + ", u.v::" + typ +
		" FROM unnest(string_to_array($2::text, chr(31))) WITH ORDINALITY AS u(v, n) WHERE $1::int = 1 ORDER BY u.n"
	arg := strings.Join(batch, "\x1f")

	// A comparison failure is only reported when the result ended cleanly: a
	// result the server cut short with an error (a literal it rejects) is a
	// rejection, whatever the rows before the error looked like.
	for route, pool := range map[string]*postgres.Pool{"cached": rig.cached, "unnamed": rig.unnamed} {
		rows, err := pool.QueryContext(ctx, q, 1, arg)
		if err != nil {
			return err
		}
		i, problem := 0, ""
		for rows.Next() {
			if problem == "" && i < len(batch) {
				problem = fuzzCompareNative(rows, route, typ, batch[i])
			}
			i++
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if problem != "" {
			return &fuzzMismatch{problem}
		}
		if i != len(batch) {
			return &fuzzMismatch{fmt.Sprintf("%s %s: %d rows for %d literals", route, typ, i, len(batch))}
		}
		checked[route] += i
	}

	srows, err := rig.db.QueryContext(ctx, q, 1, arg)
	if err != nil {
		return err
	}
	defer srows.Close()
	i, problem := 0, ""
	for srows.Next() {
		var got, want, again string
		if err := srows.Scan(&got, &want, &again); err != nil {
			if problem == "" && i < len(batch) {
				problem = fmt.Sprintf("database/sql %s: scan of %q: %v", typ, batch[i], err)
			}
		} else if (got != want || again != want) && problem == "" && i < len(batch) {
			problem = fmt.Sprintf("database/sql %s: literal %q: *string %q / %q, server ::text %q", typ, batch[i], got, again, want)
		}
		i++
	}
	if err := srows.Err(); err != nil {
		return err
	}
	if problem != "" {
		return &fuzzMismatch{problem}
	}
	if i != len(batch) {
		return &fuzzMismatch{fmt.Sprintf("database/sql %s: %d rows for %d literals", typ, i, len(batch))}
	}
	checked["database/sql"] += i
	return nil
}

// oracleExpr is the server's own text of the value: its ::text. For inet it is
// the output function (concat), because inet's ::text is text(inet), which
// always appends the mask (10.0.0.1/32) while the output function, what a
// text-format result and the simple protocol return, omits /32 and /128.
func oracleExpr(typ string) string {
	if typ == "inet" {
		return "concat(u.v::inet)"
	}
	return "u.v::" + typ + "::text"
}

// fuzzCompareNative compares the current row of a native result with the
// server's text; it returns "" when they agree.
func fuzzCompareNative(rows *postgres.Rows, route, typ, lit string) string {
	var got, want string
	var anyv any
	if err := rows.Scan(&got, &want, &anyv); err != nil {
		return fmt.Sprintf("%s %s: scan of %q: %v", route, typ, lit, err)
	}
	if got != want {
		return fmt.Sprintf("%s %s: literal %q: driver text %q, server ::text %q", route, typ, lit, got, want)
	}
	if fmt.Sprint(anyv) != want {
		return fmt.Sprintf("%s %s: literal %q: *any %T %v, server ::text %q", route, typ, lit, anyv, anyv, want)
	}
	if typ == "oid" {
		// A second Scan of the same row into the numeric destinations.
		var n int64
		var u uint32
		var again string
		if err := rows.Scan(&n, &again, &u); err != nil || strconv.FormatInt(n, 10) != want || strconv.FormatUint(uint64(u), 10) != want {
			return fmt.Sprintf("oid %s: literal %q: *int64 %d, *uint32 %d, err %v, server ::text %q", route, lit, n, u, err, want)
		}
	}
	return ""
}

// ---- generators ----

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.IntN(len(xs))] }

// fracDigits returns up to six fractional digits, trailing zeros included (the
// server prints them trimmed), or "" for none.
func fracDigits(r *rand.Rand) string {
	d := r.IntN(8) // 0, 1 .. 6, and 7 means a short fraction padded with zeros
	switch {
	case d == 0:
		return ""
	case d <= 6:
		var b strings.Builder
		b.WriteByte('.')
		for i := 0; i < d; i++ {
			b.WriteByte(byte('0' + r.IntN(10)))
		}
		return b.String()
	}
	return "." + pick(r, "5", "50", "500000", "000001", "100000", "010000", "000000")
}

func genTime(r *rand.Rand, _ int) string {
	return fmt.Sprintf("%02d:%02d:%02d%s", r.IntN(24), r.IntN(60), r.IntN(60), fracDigits(r))
}

var timeEdges = []string{
	"00:00:00", "24:00:00", "24:00:00.000000", "23:59:59.999999", "00:00:00.000001", "12:00:00",
	"00:00:00.1", "00:00:59.999999", "00:59:59", "23:00:00", "01:02:03.4", "24:00",
}

func genTimetz(r *rand.Rand, _ int) string {
	t := genTime(r, 0)
	if r.IntN(60) == 0 {
		t = "24:00:00"
	}
	sign := pick(r, "+", "-")
	h := r.IntN(16)
	switch r.IntN(4) {
	case 0:
		return fmt.Sprintf("%s%s%02d", t, sign, h)
	case 1, 2:
		return fmt.Sprintf("%s%s%02d:%02d", t, sign, h, r.IntN(60))
	}
	return fmt.Sprintf("%s%s%02d:%02d:%02d", t, sign, h, r.IntN(60), r.IntN(60))
}

var timetzEdges = []string{
	"00:00:00+00", "00:00:00-00", "24:00:00+00", "12:00:00+15:59:59", "12:00:00-15:59:59",
	"12:00:00+00:00:01", "12:00:00-00:00:01", "12:00:00+12", "12:00:00-12", "23:59:59.999999+05:30",
	"00:00:00.000001-03:30", "01:02:03.4+05:45", "12:00:00+00:01", "12:00:00-00:01", "12:00:00+01:00:01",
}

// genInterval returns a generator of interval literals whose time part never
// exceeds maxUs microseconds. PostgreSQL before 15 rejects an "N microseconds"
// field of 2^31 or more and an hour count past int32, so the large times are
// written hh:mm:ss.ffffff, and capped there.
func genInterval(maxUs int64) func(r *rand.Rand, _ int) string {
	return func(r *rand.Rand, _ int) string { return genIntervalLit(r, maxUs) }
}

func genIntervalLit(r *rand.Rand, maxUs int64) string {
	var parts []string
	field := func(small, large int64) int64 {
		switch r.IntN(6) {
		case 0:
			return 0
		case 1:
			return int64(r.IntN(3)) - 1 // -1, 0, 1: the singular
		case 2, 3:
			return r.Int64N(2*small+1) - small
		}
		return r.Int64N(2*large+1) - large
	}
	mon := field(30, math.MaxInt32)
	day := field(40, math.MaxInt32)
	if mon != 0 || r.IntN(2) == 0 {
		parts = append(parts, fmt.Sprintf("%d months", mon))
	}
	if day != 0 || r.IntN(2) == 0 {
		parts = append(parts, fmt.Sprintf("%d days", day))
	}
	var us int64
	switch r.IntN(7) {
	case 0:
	case 1:
		us = r.Int64N(2000001) - 1000000
	case 2, 3:
		us = r.Int64N(2*86400*1000000+1) - 86400*1000000
	case 4:
		us = r.Int64N(2*1000*3600*1000000+1) - 1000*3600*1000000
	default:
		us = r.Int64N(maxUs)
		if r.IntN(2) == 0 {
			us = -us
		}
	}
	// A third of the time the time part is written hh:mm:ss.ffffff.
	if us != 0 || len(parts) == 0 || r.IntN(3) == 0 {
		if r.IntN(3) == 0 || us >= 1<<31 || us <= -(1<<31) {
			sign := ""
			a := us
			if a < 0 {
				sign, a = "-", -a
			}
			parts = append(parts, fmt.Sprintf("%s%d:%02d:%02d.%06d", sign, a/3600000000, a/60000000%60, a/1000000%60, a%1000000))
		} else {
			parts = append(parts, fmt.Sprintf("%d microseconds", us))
		}
	}
	// Shuffle the field order: the input does not depend on it.
	r.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
	return strings.Join(parts, " ")
}

// intervalMaxUs is the largest time part the server's interval input accepts as
// hh:mm:ss: an hour count past int32 is an error before PostgreSQL 15.
func intervalMaxUs(pgVersionNum int) int64 {
	if pgVersionNum < 150000 {
		return (math.MaxInt32+1)*usecsPerHour - 1
	}
	return math.MaxInt64
}

const usecsPerHour = 3600 * 1000000

func intervalEdges(pgVersionNum int) []string {
	e := []string{
		"0", "00:00:00", "1 month", "-1 month", "1 day", "-1 day", "1 year", "-1 year", "1 year 1 mon 1 day 00:00:01",
		"-1 year -1 mon -1 day -00:00:01", "1 year -1 mon", "-1 year 1 mon", "1 mon -1 day 1 microsecond", "-1 mon 1 day -1 microsecond",
		"2147483647 months", "-2147483648 months", "2147483647 days", "-2147483648 days",
		"2147483647:59:59.999999", "-2147483647:59:59.999999", "2147483647 months 2147483647 days 2147483647:59:59.999999",
		"-2147483648 months -2147483648 days -2147483647:59:59.999999", "2147483647 microseconds", "-2147483647 microseconds",
		"1 microsecond", "-1 microsecond", "999999 microseconds", "-999999 microseconds", "59 minutes 59.999999 seconds",
		"100 hours", "-100 hours", "1 day -1 hour", "-1 day 1 hour", "13 months", "-13 months", "1 year 2 mons 3 days 04:05:06.789",
		"-1 year -2 mons -3 days -04:05:06.789", "3 days -04:05:06", "-3 days 04:05:06", "0.5 seconds", "1.5 days", "0.5 months",
	}
	if pgVersionNum >= 150000 {
		e = append(e, "2562047788:00:54.775807", "-2562047788:00:54.775808", "9223372036854775807 microseconds", "-9223372036854775807 microseconds",
			"2147483647 months 2147483647 days 2562047788:00:54.775807", "-2147483648 months -2147483648 days -2562047788:00:54.775808")
	}
	if pgVersionNum >= 170000 {
		e = append(e, "infinity", "-infinity")
	}
	return e
}

// ipBits returns the mask for a version: biased towards the interesting widths.
func ipMask(r *rand.Rand, width int) int {
	switch r.IntN(6) {
	case 0:
		return width
	case 1:
		return 0
	case 2:
		return pick(r, width/4, width/2, width*3/4, width-1, 1)
	}
	return r.IntN(width + 1)
}

func maskAddr(a []byte, bits int) {
	for i := range a {
		keep := bits - 8*i
		switch {
		case keep >= 8:
		case keep <= 0:
			a[i] = 0
		default:
			a[i] &^= byte(0xff >> keep)
		}
	}
}

func randByte(r *rand.Rand) byte {
	return pick(r, byte(0), 255, 127, 10, 192, 168, 1, 128, byte(r.IntN(256)), byte(r.IntN(256)))
}

func genIPv4(r *rand.Rand, cidr bool) string {
	a := []byte{randByte(r), randByte(r), randByte(r), randByte(r)}
	bits := ipMask(r, 32)
	if cidr {
		maskAddr(a, bits)
	}
	s := fmt.Sprintf("%d.%d.%d.%d", a[0], a[1], a[2], a[3])
	if r.IntN(8) == 0 && !cidr {
		return s // no mask: /32
	}
	return fmt.Sprintf("%s/%d", s, bits)
}

func genIPv6(r *rand.Rand, cidr bool) string {
	var a [16]byte
	// Groups: zero with probability 0.45 so that runs of every length occur.
	zeroP := pick(r, 2, 4, 6, 8) // out of 10
	for g := 0; g < 8; g++ {
		if r.IntN(10) < zeroP {
			continue
		}
		v := pick(r, uint16(1), 0xffff, uint16(r.IntN(65536)), uint16(r.IntN(256)), uint16(r.IntN(65536)))
		a[2*g], a[2*g+1] = byte(v>>8), byte(v)
	}
	switch r.IntN(8) {
	case 0: // IPv4-mapped
		a = [16]byte{10: 0xff, 11: 0xff, 12: randByte(r), 13: randByte(r), 14: randByte(r), 15: randByte(r)}
	case 1: // IPv4-compatible
		a = [16]byte{12: randByte(r), 13: randByte(r), 14: randByte(r), 15: randByte(r)}
	case 2: // only the last group set
		a = [16]byte{14: byte(r.IntN(256)), 15: byte(r.IntN(256))}
	}
	bits := ipMask(r, 128)
	if cidr {
		maskAddr(a[:], bits)
	}
	var b strings.Builder
	for g := 0; g < 8; g++ {
		if g > 0 {
			b.WriteByte(':')
		}
		v := uint16(a[2*g])<<8 | uint16(a[2*g+1])
		switch r.IntN(3) {
		case 0:
			fmt.Fprintf(&b, "%04x", v)
		case 1:
			fmt.Fprintf(&b, "%04X", v)
		default:
			fmt.Fprintf(&b, "%x", v)
		}
	}
	s := b.String()
	if r.IntN(8) == 0 && !cidr {
		return s
	}
	return fmt.Sprintf("%s/%d", s, bits)
}

func genInet(r *rand.Rand, _ int) string {
	if r.IntN(2) == 0 {
		return genIPv4(r, false)
	}
	return genIPv6(r, false)
}

func genCidr(r *rand.Rand, _ int) string {
	if r.IntN(2) == 0 {
		return genIPv4(r, true)
	}
	return genIPv6(r, true)
}

var inetEdges = []string{
	"0.0.0.0/0", "255.255.255.255/32", "255.255.255.255", "10.0.0.1/32", "10.0.0.1/31", "192.168.1.5/24", "127.0.0.1",
	"::", "::/0", "::1", "::1/128", "::1/127", "::2", "1::", "1::/16", "::ffff:1.2.3.4", "::ffff:0.0.0.0", "::1.2.3.4",
	"::0.0.0.2", "::ffff:ffff:ffff", "0:0:0:0:0:ffff:1.2.3.4/96", "1:0:0:2:0:0:0:3", "1:0:0:2:0:0:3:4", "1:0:2:0:3:0:4:0",
	"1:2:3:4:5:6:7:8", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/0",
	"0:0:0:0:0:0:0:1/64", "1:0:0:0:0:0:0:0", "0:1:0:0:0:0:0:0", "2001:db8::/32", "2001:db8:0:0:1:0:0:1",
}

var cidrEdges = []string{
	"0.0.0.0/0", "255.255.255.255/32", "255.255.255.255", "10.0.0.0/8", "10.1.2.3", "192.168.0.0/16", "127.0.0.1/32",
	"::/0", "::/128", "::1/128", "::1", "::", "::ffff:1.2.3.4/128", "::ffff:0.0.0.0/96", "2001:db8::/32", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128",
	"1::/16", "8000::/1", "fe80::/10",
}

func genMacaddr(r *rand.Rand, size int) string {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	if r.IntN(10) == 0 {
		for i := range b {
			b[i] = pick(r, byte(0), 255)
		}
	}
	hex := fmt.Sprintf("%02x", b)
	if r.IntN(2) == 0 {
		hex = strings.ToUpper(hex)
	}
	switch r.IntN(4) {
	case 0:
		return joinEvery(hex, 2, ":")
	case 1:
		return joinEvery(hex, 2, "-")
	case 2:
		return joinEvery(hex, 4, ".")
	}
	return hex
}

func joinEvery(s string, n int, sep string) string {
	var parts []string
	for i := 0; i < len(s); i += n {
		parts = append(parts, s[i:i+n])
	}
	return strings.Join(parts, sep)
}

var macaddrEdges = []string{"00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "08:00:2b:01:02:03", "08-00-2b-01-02-03", "08002b:010203", "0800.2b01.0203", "08002b010203", "AB:CD:EF:01:23:45"}
var macaddr8Edges = []string{"00:00:00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff:ff:ff", "08:00:2b:01:02:03:04:05", "08-00-2b-01-02-03-04-05", "0800.2b01.0203.0405", "08002b0102030405", "08:00:2b:01:02:03"}

func genOid(r *rand.Rand, _ int) string {
	switch r.IntN(5) {
	case 0:
		return strconv.FormatUint(uint64(pick(r, uint32(0), 1, 2, 16384, math.MaxInt32, math.MaxInt32+1, math.MaxUint32, math.MaxUint32-1)), 10)
	case 1:
		return strconv.Itoa(r.IntN(100000))
	}
	return strconv.FormatUint(uint64(r.Uint32()), 10)
}

var oidEdges = []string{"0", "1", "2147483647", "2147483648", "4294967295", "4294967294", "16384", "26"}

func TestBinaryTextCodecsDifferentialFuzz(t *testing.T) {
	rig := newFuzzRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var style string
	_ = rig.cached.QueryRow(ctx, "SHOW intervalstyle").Scan(&style)
	cancel()
	for i, c := range []struct {
		typ   string
		gen   func(*rand.Rand, int) string
		edges []string
	}{
		{"oid", genOid, oidEdges},
		{"time", genTime, timeEdges},
		{"timetz", genTimetz, timetzEdges},
		{"interval", genInterval(intervalMaxUs(rig.pgVersionNum)), intervalEdges(rig.pgVersionNum)},
		{"inet", genInet, inetEdges},
		{"cidr", genCidr, cidrEdges},
		{"macaddr", func(r *rand.Rand, _ int) string { return genMacaddr(r, 6) }, macaddrEdges},
		{"macaddr8", func(r *rand.Rand, _ int) string { return genMacaddr(r, 8) }, macaddr8Edges},
	} {
		t.Run(c.typ, func(t *testing.T) {
			if c.typ == "interval" && style != "postgres" {
				t.Skipf("the server's default IntervalStyle is %q: the oracle is the postgres style", style)
			}
			fuzzType(t, rig, c.typ, uint64(i+1), c.gen, c.edges)
		})
	}
}

// The IntervalStyle of the session does not change the text the binary result
// decodes to: the binary form carries no style, the driver prints the
// "postgres" form (the server's default) whatever the session says. The simple
// protocol, whose result is the server's text, follows the session's style.
func TestIntervalTextIgnoresSessionIntervalStyle(t *testing.T) {
	dsn := settingsRole(t, "intervalstyle = 'iso_8601'")
	pool := openSettingsPool(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const lit = "'1 year 2 mons -3 days 04:05:06.789'::interval"
	var style, bin, simple string
	if err := pool.QueryRow(ctx, "SHOW intervalstyle").Scan(&style); err != nil || style != "iso_8601" {
		t.Fatalf("SHOW intervalstyle = %q, %v: the role setting did not apply", style, err)
	}
	if err := pool.QueryRow(ctx, "SELECT "+lit+" WHERE $1::int = 1", 1).Scan(&bin); err != nil {
		t.Fatal(err)
	}
	if want := "1 year 2 mons -3 days +04:05:06.789"; bin != want {
		t.Errorf("binary result under iso_8601: got %q, want the postgres form %q", bin, want)
	}
	if err := pool.QueryRow(ctx, "SELECT "+lit).Scan(&simple); err != nil {
		t.Fatal(err)
	}
	if want := "P1Y2M-3DT4H5M6.789S"; simple != want {
		t.Errorf("simple protocol under iso_8601: got %q, want the server's text %q", simple, want)
	}
}
