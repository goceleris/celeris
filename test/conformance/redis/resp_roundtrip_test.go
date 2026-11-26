//go:build redis

package redis_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// This file reads every RESP reply type back from a real server through the
// public API (Client.Do and the typed helpers), over RESP3 (the default) and
// RESP2 (WithForceRESP2), and compares the decoded Value, rendered as a
// string, with a literal. The RESP3-only types have no ordinary command that
// produces them (a double, a boolean, a big number, a verbatim string), so
// they come from EVAL: redis.setresp(3) makes a script return them, and a
// RESP2 client receives the server's own downgrade (bulk, integer, flat
// array), which is the second column of expectations.
//
// Cells that fail today carry a bug id (rtKnownBug) and run only with
// `-tags 'redis redisrtbugs'`.

// renderValue is the string form the expectations use.
func renderValue(v celredis.Value) string {
	switch v.Type {
	case celredis.TySimple:
		return "simple(" + string(v.Str) + ")"
	case celredis.TyError:
		return "error(" + string(v.Str) + ")"
	case celredis.TyInt:
		return "int(" + strconv.FormatInt(v.Int, 10) + ")"
	case celredis.TyBulk:
		return "bulk(" + string(v.Str) + ")"
	case celredis.TyNull:
		return "null"
	case celredis.TyBool:
		return "bool(" + strconv.FormatBool(v.Bool) + ")"
	case celredis.TyDouble:
		return "double(" + strconv.FormatFloat(v.Float, 'g', -1, 64) + ")"
	case celredis.TyBigInt:
		return "big(" + string(v.BigN) + ")"
	case celredis.TyVerbatim:
		return "verbatim(" + string(v.Str) + ")"
	case celredis.TyArray, celredis.TySet, celredis.TyPush:
		name := map[celredis.Type]string{celredis.TyArray: "array", celredis.TySet: "set", celredis.TyPush: "push"}[v.Type]
		parts := make([]string, len(v.Array))
		for i, e := range v.Array {
			parts[i] = renderValue(e)
		}
		return name + "[" + strings.Join(parts, ",") + "]"
	case celredis.TyMap, celredis.TyAttr:
		parts := make([]string, len(v.Map))
		for i, kv := range v.Map {
			parts[i] = renderValue(kv.K) + "=>" + renderValue(kv.V)
		}
		name := "map"
		if v.Type == celredis.TyAttr {
			name = "attr"
		}
		return name + "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprintf("unknown(type %d)", v.Type)
}

// rtClients runs f once on a RESP3 client and once on a RESP2 client.
func rtClients(t *testing.T, f func(t *testing.T, c *celredis.Client, resp int)) {
	t.Helper()
	for _, resp := range []int{3, 2} {
		t.Run(fmt.Sprintf("resp%d", resp), func(t *testing.T) {
			var c *celredis.Client
			if resp == 2 {
				c = connectClient(t, celredis.WithForceRESP2())
			} else {
				c = connectClient(t)
			}
			if c.Proto() != resp {
				t.Fatalf("negotiated RESP%d, want RESP%d", c.Proto(), resp)
			}
			f(t, c, resp)
		})
	}
}

// rtReplyCell is one reply type read through Client.Do: the command, and the
// reply rendered (renderValue) on RESP3 and on RESP2.
type rtReplyCell struct {
	name string
	args []any
	r3   string // expected render on RESP3
	r2   string // expected render on RESP2
}

// rtReplyCells lists the reply-type cells TestRESPReplyTypes runs. k is the
// key prefix of the test; kl, kh, ks and kz name the list, hash, set and
// sorted set its setup fills.
func rtReplyCells(k string) []rtReplyCell {
	const set3 = "redis.setresp(3); "
	kl, kh, ks, kz := k+":l", k+":h", k+":s", k+":z"
	return []rtReplyCell{
		{"simple_string_PING", []any{"PING"}, "simple(PONG)", "simple(PONG)"},
		{"simple_string_SET", []any{"SET", k + ":n", "1"}, "simple(OK)", "simple(OK)"},
		{"simple_string_status_reply", []any{"EVAL", "return redis.status_reply('FINE')", 0}, "simple(FINE)", "simple(FINE)"},
		{"integer", []any{"INCR", k + ":max"}, "int(9223372036854775807)", "int(9223372036854775807)"},
		{"integer_negative", []any{"TTL", k + ":nosuch"}, "int(-2)", "int(-2)"},
		{"integer_zero", []any{"EXISTS", k + ":nosuch"}, "int(0)", "int(0)"},
		{"bulk", []any{"GET", k}, "bulk(v)", "bulk(v)"},
		{"bulk_empty", []any{"GET", k + ":e"}, "bulk()", "bulk()"},
		{"bulk_unicode", []any{"ECHO", "héllo 世界 🚀"}, "bulk(héllo 世界 🚀)", "bulk(héllo 世界 🚀)"},
		{"null_bulk", []any{"GET", k + ":nosuch"}, "null", "null"},
		{"null_array_BLPOP_timeout", []any{"BLPOP", k + ":nosuch", "0.01"}, "null", "null"},
		{"array", []any{"LRANGE", kl, 0, -1}, "array[bulk(a),bulk(b)]", "array[bulk(a),bulk(b)]"},
		{"array_empty", []any{"LRANGE", k + ":nosuch", 0, -1}, "array[]", "array[]"},
		{"array_with_nulls", []any{"MGET", k, k + ":nosuch", k}, "array[bulk(v),null,bulk(v)]", "array[bulk(v),null,bulk(v)]"},
		{"array_nested", []any{"EVAL", "return {1,{2,{3,'x'}},{}}", 0}, "array[int(1),array[int(2),array[int(3),bulk(x)]],array[]]", "array[int(1),array[int(2),array[int(3),bulk(x)]],array[]]"},
		{"map", []any{"HGETALL", kh}, "map{bulk(f1)=>bulk(v1),bulk(f2)=>bulk(v2)}", "array[bulk(f1),bulk(v1),bulk(f2),bulk(v2)]"},
		{"map_int_values", []any{"EVAL", set3 + "return {map={a=1}}", 0}, "map{bulk(a)=>int(1)}", "array[bulk(a),int(1)]"},
		{"set", []any{"SMEMBERS", ks}, "set[bulk(m1)]", "array[bulk(m1)]"},
		{"double", []any{"ZSCORE", kz, "x"}, "double(1.5)", "bulk(1.5)"},
		{"double_script", []any{"EVAL", set3 + "return {double=3.14}", 0}, "double(3.14)", "bulk(3.14)"},
		{"double_inf", []any{"ZSCORE", kz, "pinf"}, "double(+Inf)", "bulk(inf)"},
		{"double_neg_inf", []any{"ZSCORE", kz, "ninf"}, "double(-Inf)", "bulk(-inf)"},
		{"double_in_array_WITHSCORES", []any{"ZRANGE", kz, 0, -1, "WITHSCORES"}, "array[array[bulk(ninf),double(-Inf)],array[bulk(x),double(1.5)],array[bulk(pinf),double(+Inf)]]", "array[bulk(ninf),bulk(-inf),bulk(x),bulk(1.5),bulk(pinf),bulk(inf)]"},
		{"bool_true", []any{"EVAL", set3 + "return true", 0}, "bool(true)", "int(1)"},
		{"bool_false", []any{"EVAL", set3 + "return false", 0}, "bool(false)", "int(0)"},
		{"big_number", []any{"EVAL", set3 + "return {big_number='123456789012345678901234567890'}", 0}, "big(123456789012345678901234567890)", "bulk(123456789012345678901234567890)"},
		{"big_number_negative", []any{"EVAL", set3 + "return {big_number='-99999999999999999999'}", 0}, "big(-99999999999999999999)", "bulk(-99999999999999999999)"},
		{"verbatim_string", []any{"EVAL", set3 + "return {verbatim_string={string='hello', format='txt'}}", 0}, "verbatim(txt:hello)", "bulk(hello)"},
	}
}

// TestRESPReplyTypes: one command per reply type, both protocols.
func TestRESPReplyTypes(t *testing.T) {
	rtClients(t, func(t *testing.T, c *celredis.Client, resp int) {
		ctx := ctxWithTimeout(t, 20*time.Second)
		k := uniqueKey(t, "rt")
		kl, kh, ks, kz := k+":l", k+":h", k+":s", k+":z"
		cleanupKeys(t, c, k, kl, kh, ks, kz, k+":n", k+":e", k+":max")
		for _, setup := range [][]any{
			{"RPUSH", kl, "a", "b"},
			{"HSET", kh, "f1", "v1", "f2", "v2"},
			{"SADD", ks, "m1"},
			{"ZADD", kz, "+inf", "pinf", "-inf", "ninf", "1.5", "x"},
			{"SET", k, "v"},
			{"SET", k + ":e", ""},
			{"SET", k + ":max", "9223372036854775806"},
		} {
			if _, err := c.Do(ctx, setup...); err != nil {
				t.Fatalf("setup %v: %v", setup, err)
			}
		}
		cells := rtReplyCells(k)
		for _, cl := range cells {
			t.Run(cl.name, func(t *testing.T) {
				v, err := c.Do(ctx, cl.args...)
				if err != nil {
					t.Fatalf("Do(%v): %v", cl.args[:1], err)
				}
				want := cl.r3
				if resp == 2 {
					want = cl.r2
				}
				if got := renderValue(*v); got != want {
					t.Fatalf("Do(%v) on RESP%d: got %s, want %s", cl.args[:1], resp, got, want)
				}
			})
		}

		// A server-sent verbatim string (LOLWUT, INFO, CLIENT INFO) is a
		// verbatim on RESP3 and a bulk on RESP2.
		t.Run("verbatim_string_from_command", func(t *testing.T) {
			v, err := c.Do(ctx, "INFO", "server")
			if err != nil {
				t.Fatal(err)
			}
			wantType, prefix := celredis.TyBulk, "# Server"
			if resp == 3 {
				wantType, prefix = celredis.TyVerbatim, "txt:# Server"
			}
			if v.Type != wantType || !strings.HasPrefix(string(v.Str), prefix) {
				t.Fatalf("INFO on RESP%d: type %d str %.20q, want type %d prefix %q", resp, v.Type, v.Str, wantType, prefix)
			}
			s, err := c.DoString(ctx, "INFO", "server")
			if err != nil || !strings.HasPrefix(s, "# Server") {
				t.Fatalf("DoString(INFO) = %.20q, %v; want the text without the txt: prefix", s, err)
			}
		})

		// Server errors: a simple error reply is a *RedisError carrying the prefix.
		for _, e := range []struct {
			name   string
			args   []any
			prefix string
		}{
			{"wrongtype", []any{"GET", kl}, "WRONGTYPE"},
			{"unknown_command", []any{"NOSUCHCOMMAND"}, "ERR"},
			{"wrong_arity", []any{"GET"}, "ERR"},
			{"script_error_reply", []any{"EVAL", "return redis.error_reply('MYERR boom')", 0}, "MYERR"},
			{"not_an_integer", []any{"INCR", k}, "ERR"},
		} {
			t.Run("error_"+e.name, func(t *testing.T) {
				v, err := c.Do(ctx, e.args...)
				var re *celredis.RedisError
				if !errors.As(err, &re) {
					t.Fatalf("Do(%v) = %v, %v; want a *RedisError", e.args[:1], v, err)
				}
				if re.Prefix != e.prefix {
					t.Fatalf("prefix %q, want %q (%v)", re.Prefix, e.prefix, err)
				}
				// The connection is still good after an error reply.
				if err := c.Ping(ctx); err != nil {
					t.Fatalf("Ping after an error reply: %v", err)
				}
			})
		}
	})
}

// rtBulkSizes are the bulk value sizes TestRESPLargeAndBinaryBulk always runs.
var rtBulkSizes = []int{1<<20 - 1, 1 << 20, 1<<20 + 1, 3<<20 + 13}

// rtBulkSizesOverLinuxCap are the sizes that fail today (R3), so they run only
// with the redisrtbugs tag: on Linux the event loop caps one outbound write at
// 4 MiB (driver/internal/eventloop maxPendingBytes), so a SET of a value of
// 4 MiB or more fails with ErrQueueFull; Redis takes 512 MiB.
var rtBulkSizesOverLinuxCap = []int{4<<20 + 13, 17 << 20}

// TestRESPLargeAndBinaryBulk: a bulk reply larger than the reader's 1 MiB
// inline limit is copied out of the buffer, and CRLF, '$' and '+' inside a
// bulk must not be taken for framing.
func TestRESPLargeAndBinaryBulk(t *testing.T) {
	rtClients(t, func(t *testing.T, c *celredis.Client, resp int) {
		ctx := ctxWithTimeout(t, 60*time.Second)
		k := uniqueKey(t, "bulk")
		cleanupKeys(t, c, k)

		all := make([]byte, 256)
		for i := range all {
			all[i] = byte(i)
		}
		framing := append(append([]byte{}, all...), "\r\n$5\r\nhello\r\n+OK\r\n:1\r\n*1\r\n"...)
		payloads := map[string][]byte{"all_bytes_and_framing": framing}
		sizes := append([]int{}, rtBulkSizes...)
		if rtKnownBugs {
			sizes = append(sizes, rtBulkSizesOverLinuxCap...)
		}
		for _, n := range sizes {
			payloads["size_"+strconv.Itoa(n)] = bytes.Repeat([]byte("0123456789abcdef\r\n"), n/18+1)[:n]
		}
		for name, p := range payloads {
			t.Run(name, func(t *testing.T) {
				if err := c.SetBytes(ctx, k, p, 0); err != nil {
					t.Fatalf("SetBytes: %v", err)
				}
				got, err := c.GetBytes(ctx, k)
				if err != nil {
					t.Fatalf("GetBytes: %v", err)
				}
				if !bytes.Equal(got, p) {
					t.Fatalf("GetBytes: %d bytes back, want %d, equal=false", len(got), len(p))
				}
				s, err := c.Get(ctx, k)
				if err != nil || s != string(p) {
					t.Fatalf("Get: len %d, err %v; want %d bytes", len(s), err, len(p))
				}
				v, err := c.Do(ctx, "GET", k)
				if err != nil || v.Type != celredis.TyBulk || !bytes.Equal(v.Str, p) {
					t.Fatalf("Do(GET): %v, %v", err, v != nil && v.Type == celredis.TyBulk)
				}
				if n, err := c.DoInt(ctx, "STRLEN", k); err != nil || n != int64(len(p)) {
					t.Fatalf("STRLEN = %d, %v; want %d", n, err, len(p))
				}
			})
		}
		// The same connection still frames correctly after the large replies.
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("Ping after large replies: %v", err)
		}
	})
}

const (
	bStr   = "R1 DoString/DoInt on a RESP3 double, bool or big number returns an error where RESP2 returns the value"
	bSlice = "R2 DoSlice turns every non-bulk element (integer, double, bool, nested array) into an empty string"
	bMap   = "R1 DoSlice on a RESP3 map returns an error where RESP2 returns the flat array"
)

// rtHelperRow is one reply, read through the typed helpers of Client: what each of DoString,
// DoInt, DoBool and DoSlice must give ("ERR" when the helper must return an
// error: the reply is not of its kind).
type rtHelperRow struct {
	name, script                 string
	str, integer, boolean, slice string // "ERR" or the expected rendering; slice is joined with "|"
	bug3, bug2                   string // known bug on RESP3 / on RESP2: the cell is run only with redisrtbugs
}

// rtHelperRows lists the rows TestRESPTypedHelpers runs.
func rtHelperRows() []rtHelperRow {
	const set3 = "redis.setresp(3); "
	return []rtHelperRow{
		{name: "bulk", script: "return 'bulk'", str: "bulk", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "integer", script: "return 7", str: "7", integer: "7", boolean: "true", slice: "ERR"},
		{name: "status", script: "return redis.status_reply('FINE')", str: "FINE", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "double", script: set3 + "return {double=3.5}", str: "3.5", integer: "ERR", boolean: "ERR", slice: "ERR", bug3: bStr},
		{name: "bool_true", script: set3 + "return true", str: "1", integer: "1", boolean: "true", slice: "ERR", bug3: bStr},
		{name: "bool_false", script: set3 + "return false", str: "0", integer: "0", boolean: "false", slice: "ERR", bug3: bStr},
		{name: "big_number", script: set3 + "return {big_number='123456789012345678901234567890'}", str: "123456789012345678901234567890", integer: "ERR", boolean: "ERR", slice: "ERR", bug3: bStr},
		{name: "verbatim", script: set3 + "return {verbatim_string={string='hello', format='txt'}}", str: "hello", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "set", script: set3 + "return {set={a=true}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "a"},
		{name: "array_of_bulk", script: "return {'x','y'}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "x|y"},
		{name: "array_of_int", script: "return {1,2}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "1|2", bug3: bSlice, bug2: bSlice},
		{name: "map_flat_on_resp2", script: set3 + "return {map={a=1}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "a|1", bug3: bMap + "; " + bSlice, bug2: bSlice},
	}
}

// TestRESPTypedHelpers: the typed helpers read the same reply the same way on
// either protocol. Each row is a script whose reply is one RESP3 type; the
// expectation is what RESP2 (the server's own downgrade) gives, and RESP3 must
// agree. "ERR" means the helper returns an error.
func TestRESPTypedHelpers(t *testing.T) {
	rows := rtHelperRows()
	rtClients(t, func(t *testing.T, c *celredis.Client, resp int) {
		ctx := ctxWithTimeout(t, 20*time.Second)
		show := func(s string, err error) string {
			if err != nil {
				return "ERR"
			}
			return s
		}
		for _, r := range rows {
			bug := r.bug3
			if resp == 2 {
				bug = r.bug2
			}
			if bug != "" && !rtKnownBugs {
				continue
			}
			t.Run(r.name, func(t *testing.T) {
				if bug != "" {
					t.Logf("known bug: %s", bug)
				}
				args := []any{"EVAL", r.script, 0}
				s, err := c.DoString(ctx, args...)
				if got := show(s, err); got != r.str {
					t.Errorf("DoString = %q, want %q", got, r.str)
				}
				n, err := c.DoInt(ctx, args...)
				if got := show(strconv.FormatInt(n, 10), err); got != r.integer {
					t.Errorf("DoInt = %q, want %q", got, r.integer)
				}
				b, err := c.DoBool(ctx, args...)
				if got := show(strconv.FormatBool(b), err); got != r.boolean {
					t.Errorf("DoBool = %q, want %q", got, r.boolean)
				}
				sl, err := c.DoSlice(ctx, args...)
				if got := show(strings.Join(sl, "|"), err); got != r.slice {
					t.Errorf("DoSlice = %q, want %q", got, r.slice)
				}
			})
		}
	})
}

// TestRESPTypedDoubles: the commands that reply with a double (RESP3) or a
// numeric bulk (RESP2) decode to the same float64 on both, infinities included.
func TestRESPTypedDoubles(t *testing.T) {
	rtClients(t, func(t *testing.T, c *celredis.Client, resp int) {
		ctx := ctxWithTimeout(t, 20*time.Second)
		k := uniqueKey(t, "dbl")
		cleanupKeys(t, c, k+":z", k+":f", k+":h")
		if _, err := c.ZAdd(ctx, k+":z", celredis.Z{Score: math.Inf(1), Member: "pinf"}, celredis.Z{Score: math.Inf(-1), Member: "ninf"}, celredis.Z{Score: 1.5, Member: "x"}, celredis.Z{Score: -0.25, Member: "neg"}); err != nil {
			t.Fatalf("ZAdd: %v", err)
		}
		for member, want := range map[string]float64{"x": 1.5, "neg": -0.25, "pinf": math.Inf(1), "ninf": math.Inf(-1)} {
			got, err := c.ZScore(ctx, k+":z", member)
			if err != nil || got != want {
				t.Errorf("ZScore(%s) = %v, %v; want %v", member, got, err, want)
			}
		}
		if _, err := c.ZScore(ctx, k+":z", "nosuch"); !errors.Is(err, celredis.ErrNil) {
			t.Errorf("ZScore(missing) err = %v, want ErrNil", err)
		}
		// INCRBYFLOAT replies with a bulk on both protocols.
		for _, step := range []struct{ by, want float64 }{{1.5, 1.5}, {2.25, 3.75}, {-3.75, 0}, {1e300, 1e300}, {-1e300, 0}, {0.1, 0.1}} {
			got, err := c.IncrByFloat(ctx, k+":f", step.by)
			if err != nil || got != step.want {
				t.Errorf("IncrByFloat(%v) = %v, %v; want %v", step.by, got, err, step.want)
			}
		}
		hg, err := c.HIncrByFloat(ctx, k+":h", "f", -2.75)
		if err != nil || hg != -2.75 {
			t.Errorf("HIncrByFloat = %v, %v; want -2.75", hg, err)
		}
		zi, err := c.ZIncrBy(ctx, k+":z", 0.5, "x")
		if err != nil || zi != 2 {
			t.Errorf("ZIncrBy = %v, %v; want 2", zi, err)
		}
	})
}

// TestRESPTypedAggregates: map, set and array replies through the typed
// helpers, including values that are not ASCII and fields that are empty.
func TestRESPTypedAggregates(t *testing.T) {
	rtClients(t, func(t *testing.T, c *celredis.Client, resp int) {
		ctx := ctxWithTimeout(t, 20*time.Second)
		k := uniqueKey(t, "agg")
		cleanupKeys(t, c, k+":h", k+":s", k+":l", k+":empty")
		if _, err := c.HSet(ctx, k+":h", "a", "1", "b", "", "ünï", "世界"); err != nil {
			t.Fatal(err)
		}
		m, err := c.HGetAll(ctx, k+":h")
		if err != nil || len(m) != 3 || m["a"] != "1" || m["b"] != "" || m["ünï"] != "世界" {
			t.Fatalf("HGetAll = %v, %v", m, err)
		}
		if m, err := c.HGetAll(ctx, k+":empty"); err != nil || len(m) != 0 {
			t.Fatalf("HGetAll(missing) = %v, %v; want an empty map", m, err)
		}
		if _, err := c.SAdd(ctx, k+":s", "x", "y", "z", ""); err != nil {
			t.Fatal(err)
		}
		sm, err := c.SMembers(ctx, k+":s")
		if err != nil || len(sm) != 4 {
			t.Fatalf("SMembers = %v, %v", sm, err)
		}
		seen := map[string]bool{}
		for _, s := range sm {
			seen[s] = true
		}
		for _, w := range []string{"x", "y", "z", ""} {
			if !seen[w] {
				t.Errorf("SMembers lacks %q: %v", w, sm)
			}
		}
		if sm, err := c.SMembers(ctx, k+":empty"); err != nil || len(sm) != 0 {
			t.Fatalf("SMembers(missing) = %v, %v; want empty", sm, err)
		}
		if ok, err := c.SIsMember(ctx, k+":s", "x"); err != nil || !ok {
			t.Fatalf("SIsMember(x) = %v, %v", ok, err)
		}
		if ok, err := c.SIsMember(ctx, k+":s", "nope"); err != nil || ok {
			t.Fatalf("SIsMember(nope) = %v, %v", ok, err)
		}
		if _, err := c.RPush(ctx, k+":l", "1", "", "三"); err != nil {
			t.Fatal(err)
		}
		lr, err := c.LRange(ctx, k+":l", 0, -1)
		if err != nil || strings.Join(lr, "|") != "1||三" {
			t.Fatalf("LRange = %q, %v", lr, err)
		}
		if err := c.MSet(ctx, k+":m1", "one", k+":m2", "two"); err != nil {
			t.Fatal(err)
		}
		cleanupKeys(t, c, k+":m1", k+":m2")
		mg, err := c.MGet(ctx, k+":m1", k+":nosuch", k+":m2")
		if err != nil || len(mg) != 3 || mg[0] != "one" || mg[2] != "two" {
			t.Fatalf("MGet = %q, %v", mg, err)
		}
	})
}

// TestRESPPushOnCommandConn: a RESP3 push frame (client-side caching
// invalidation) that arrives on a command connection is delivered to the
// WithOnPush callback and does not disturb the reply stream.
func TestRESPPushOnCommandConn(t *testing.T) {
	type push struct {
		kind string
		data []celredis.Value
	}
	var mu sync.Mutex
	var pushes []push
	c := connectClient(t,
		celredis.WithPoolSize(1), // one command connection: tracking is per connection
		celredis.WithOnPush(func(kind string, data []celredis.Value) {
			mu.Lock()
			defer mu.Unlock()
			cp := make([]celredis.Value, len(data))
			for i, d := range data {
				cp[i] = d
				cp[i].Str = append([]byte(nil), d.Str...)
				cp[i].Array = append([]celredis.Value(nil), d.Array...)
				for j := range cp[i].Array {
					cp[i].Array[j].Str = append([]byte(nil), d.Array[j].Str...)
				}
			}
			pushes = append(pushes, push{kind, cp})
		}))
	if c.Proto() != 3 {
		t.Fatalf("negotiated RESP%d, want RESP3", c.Proto())
	}
	ctx := ctxWithTimeout(t, 20*time.Second)
	k := uniqueKey(t, "push")
	cleanupKeys(t, c, k)
	if _, err := c.Do(ctx, "CLIENT", "TRACKING", "ON"); err != nil {
		t.Fatalf("CLIENT TRACKING ON: %v", err)
	}
	if err := c.Set(ctx, k, "1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, k); err != nil { // the read registers the key for tracking
		t.Fatal(err)
	}
	other := connectClient(t)
	if err := other.Set(ctx, k, "2", 0); err != nil {
		t.Fatal(err)
	}
	// The invalidation arrives between replies. Keep issuing commands on the
	// tracked connection until it is seen: each reply must still be correct.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, err := c.Get(ctx, k); err != nil || got != "2" {
			t.Fatalf("Get after the other client's write = %q, %v; want 2", got, err)
		}
		mu.Lock()
		n := len(pushes)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no invalidate push within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	p := pushes[0]
	if p.kind != "invalidate" {
		t.Fatalf("push kind %q, want invalidate", p.kind)
	}
	if len(p.data) != 1 || len(p.data[0].Array) != 1 || string(p.data[0].Array[0].Str) != k {
		t.Fatalf("push data = %s, want the invalidated key %q", renderValue(p.data[0]), k)
	}
}
