//go:build redis

package redis_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// dfRow is one command read through DoString, DoInt, DoBool and DoSlice on a
// RESP3 client (the default) and on a RESP2 client (WithForceRESP2). The two
// must give the same value. The str, integer and slice fields pin what the
// RESP2 client must give ("ERR" for an error); an empty field pins nothing,
// and then only the comparison with RESP3 runs. A RESP3 error must name the
// reply type, so a reply the helper cannot read is never an empty string.
type dfRow struct {
	name                string
	args                []any
	str, integer, slice string
}

// dfKeys are the keys the rows read; TestRESPDoHelpersMatchAcrossProtocols
// fills them.
type dfKeys struct {
	ord, big, small, special, one, set, hash, hone, m1, m2 string
}

func dfRows(k dfKeys) []dfRow {
	const set3 = "redis.setresp(3); "
	return []dfRow{
		// Sorted-set replies. RESP3 sends the scores as doubles, in pairs;
		// RESP2 sends a flat list of bulk strings.
		{name: "zrange_withscores", args: []any{"ZRANGE", k.ord, 0, -1, "WITHSCORES"}, str: "ERR", integer: "ERR", slice: "d|-2.25|c|0.1|a|1.5|b|100"},
		{name: "zrange_withscores_lowercase", args: []any{"zrange", k.ord, 0, -1, "withscores"}, str: "ERR", integer: "ERR", slice: "d|-2.25|c|0.1|a|1.5|b|100"},
		{name: "zrangebyscore_withscores", args: []any{"ZRANGEBYSCORE", k.ord, "-inf", "+inf", "WITHSCORES"}, str: "ERR", integer: "ERR", slice: "d|-2.25|c|0.1|a|1.5|b|100"},
		{name: "zrevrange_withscores", args: []any{"ZREVRANGE", k.ord, 0, -1, "WITHSCORES"}, str: "ERR", integer: "ERR", slice: "b|100|a|1.5|c|0.1|d|-2.25"},
		{name: "zrange_withscores_large_scores", args: []any{"ZRANGE", k.big, 0, -1, "WITHSCORES"}, str: "ERR", integer: "ERR"},
		{name: "zrange_withscores_small_scores", args: []any{"ZRANGE", k.small, 0, -1, "WITHSCORES"}, str: "ERR", integer: "ERR"},
		{name: "zrange_withscores_inf_and_zero", args: []any{"ZRANGE", k.special, 0, -1, "WITHSCORES"}, str: "ERR", integer: "ERR"},
		{name: "zrandmember_withscores", args: []any{"ZRANDMEMBER", k.one, 1, "WITHSCORES"}, str: "ERR", integer: "ERR", slice: "only|2.5"},
		{name: "hrandfield_withvalues", args: []any{"HRANDFIELD", k.hone, 1, "WITHVALUES"}, str: "ERR", integer: "ERR", slice: "f|v"},
		{name: "zmscore_with_a_missing_member", args: []any{"ZMSCORE", k.ord, "a", "nosuch", "b"}, str: "ERR", integer: "ERR", slice: "1.5||100"},
		{name: "zscore_fraction", args: []any{"ZSCORE", k.ord, "a"}, str: "1.5", integer: "ERR", slice: "ERR"},
		{name: "zscore_integral", args: []any{"ZSCORE", k.ord, "b"}, str: "100", integer: "100", slice: "ERR"},
		{name: "zscore_negative", args: []any{"ZSCORE", k.ord, "d"}, str: "-2.25", integer: "ERR", slice: "ERR"},
		{name: "zscore_1e6", args: []any{"ZSCORE", k.big, "m6"}, str: "", integer: "", slice: "ERR"},
		{name: "zscore_1_7e9", args: []any{"ZSCORE", k.big, "m9"}, str: "", integer: "", slice: "ERR"},
		{name: "zscore_1e17", args: []any{"ZSCORE", k.big, "m17"}, str: "", integer: "", slice: "ERR"},
		{name: "zscore_1e20", args: []any{"ZSCORE", k.big, "m20"}, str: "", integer: "", slice: "ERR"},
		{name: "zscore_1e21", args: []any{"ZSCORE", k.big, "m21"}, str: "", integer: "", slice: "ERR"},
		{name: "zscore_1e-4", args: []any{"ZSCORE", k.small, "s4"}, str: "0.0001", integer: "ERR", slice: "ERR"},
		{name: "zscore_1e-5", args: []any{"ZSCORE", k.small, "s5"}, str: "", integer: "ERR", slice: "ERR"},
		{name: "zscore_1e-7", args: []any{"ZSCORE", k.small, "s7"}, str: "", integer: "ERR", slice: "ERR"},
		{name: "zscore_1_5e-10", args: []any{"ZSCORE", k.small, "s10"}, str: "", integer: "ERR", slice: "ERR"},
		{name: "zscore_inf", args: []any{"ZSCORE", k.special, "pinf"}, str: "inf", integer: "ERR", slice: "ERR"},
		{name: "zscore_neg_inf", args: []any{"ZSCORE", k.special, "ninf"}, str: "-inf", integer: "ERR", slice: "ERR"},
		{name: "zscore_zero", args: []any{"ZSCORE", k.special, "zero"}, str: "0", integer: "0", slice: "ERR"},
		{name: "zscore_negative_zero", args: []any{"ZSCORE", k.special, "nzero"}, str: "", integer: "", slice: "ERR"},

		// Set, hash, config and MGET replies.
		{name: "smismember", args: []any{"SMISMEMBER", k.set, "x", "nosuch", "y"}, str: "ERR", integer: "ERR", slice: "1|0|1"},
		{name: "smembers_one_member", args: []any{"SMEMBERS", k.hone + ":s"}, str: "ERR", integer: "ERR", slice: "m"},
		{name: "hgetall", args: []any{"HGETALL", k.hash}, str: "ERR", integer: "ERR", slice: "f1|v1|f2|v2"},
		{name: "config_get", args: []any{"CONFIG", "GET", "databases"}, str: "ERR", integer: "ERR", slice: "databases|16"},
		{name: "mget_with_a_nil", args: []any{"MGET", k.m1, k.m1 + ":nosuch", k.m2}, str: "ERR", integer: "ERR", slice: "one||two"},

		// Scripts: mixed types in one reply.
		{name: "lua_array_with_a_nil", args: []any{"EVAL", "return {'a', false, 'b'}", 0}, str: "ERR", integer: "ERR", slice: "a||b"},
		{name: "lua_mixed_types", args: []any{"EVAL", "return {1, 'two', 3, true}", 0}, str: "ERR", integer: "ERR", slice: "1|two|3|1"},
		{name: "lua_mixed_types_resp3", args: []any{"EVAL", set3 + "return {1, {double=2.5}, true, {big_number='12345678901234567890'}, 'x'}", 0}, str: "ERR", integer: "ERR", slice: "1|2.5|1|12345678901234567890|x"},
		{name: "lua_nested_array", args: []any{"EVAL", "return {'x',{'y'}}", 0}, str: "ERR", integer: "ERR", slice: "ERR"},
		{name: "lua_nested_pairs_without_an_option", args: []any{"EVAL", "return {{'a','b'},{'c','d'}}", 0}, str: "ERR", integer: "ERR", slice: "ERR"},
		{name: "lua_array_nested_two_deep", args: []any{"EVAL", "return {1,{2,{3}}}", 0}, str: "ERR", integer: "ERR", slice: "ERR"},
		{name: "lua_double", args: []any{"EVAL", set3 + "return {double=3.5}", 0}, str: "3.5", integer: "ERR", slice: "ERR"},
		{name: "lua_double_integral", args: []any{"EVAL", set3 + "return {double=3}", 0}, str: "3", integer: "3", slice: "ERR"},
		{name: "lua_bool_true", args: []any{"EVAL", set3 + "return true", 0}, str: "1", integer: "1", slice: "ERR"},
		{name: "lua_bool_false", args: []any{"EVAL", set3 + "return false", 0}, str: "0", integer: "0", slice: "ERR"},
		{name: "lua_big_number_fits_int64", args: []any{"EVAL", set3 + "return {big_number='42'}", 0}, str: "42", integer: "42", slice: "ERR"},
		{name: "lua_big_number", args: []any{"EVAL", set3 + "return {big_number='123456789012345678901234567890'}", 0}, str: "123456789012345678901234567890", integer: "ERR", slice: "ERR"},
		{name: "lua_map", args: []any{"EVAL", set3 + "return {map={a=1}}", 0}, str: "ERR", integer: "ERR", slice: "a|1"},
		{name: "lua_map_with_a_double", args: []any{"EVAL", set3 + "return {map={a={double=1.5}}}", 0}, str: "ERR", integer: "ERR", slice: "a|1.5"},
		{name: "lua_set", args: []any{"EVAL", set3 + "return {set={a=true}}", 0}, str: "ERR", integer: "ERR", slice: "a"},
	}
}

// TestRESPDoHelpersMatchAcrossProtocols: for each row of dfRows, DoString,
// DoInt, DoBool and DoSlice return the same value on a default (RESP3) client
// as on a RESP2 client, and where the helper cannot read the reply the RESP3
// error names the reply type.
func TestRESPDoHelpersMatchAcrossProtocols(t *testing.T) {
	c3 := connectClient(t)
	c2 := connectClient(t, celredis.WithForceRESP2())
	if c3.Proto() != 3 || c2.Proto() != 2 {
		t.Fatalf("negotiated RESP%d and RESP%d, want RESP3 and RESP2", c3.Proto(), c2.Proto())
	}
	ctx := ctxWithTimeout(t, 60*time.Second)
	base := uniqueKey(t, "df")
	k := dfKeys{
		ord: base + ":ord", big: base + ":big", small: base + ":small", special: base + ":special", one: base + ":one",
		set: base + ":set", hash: base + ":hash", hone: base + ":hone", m1: base + ":m1", m2: base + ":m2",
	}
	cleanupKeys(t, c3, k.ord, k.big, k.small, k.special, k.one, k.set, k.hash, k.hone, k.hone+":s", k.m1, k.m2)
	for _, setup := range [][]any{
		{"ZADD", k.ord, "1.5", "a", "100", "b", "0.1", "c", "-2.25", "d"},
		{"ZADD", k.big, "1000000", "m6", "1700000000", "m9", "1e17", "m17", "1e20", "m20", "1e21", "m21"},
		{"ZADD", k.small, "0.0001", "s4", "1e-5", "s5", "1e-7", "s7", "1.5e-10", "s10"},
		{"ZADD", k.special, "+inf", "pinf", "-inf", "ninf", "0", "zero", "-0", "nzero"},
		{"ZADD", k.one, "2.5", "only"},
		{"SADD", k.set, "x", "y"},
		{"HSET", k.hash, "f1", "v1", "f2", "v2"},
		{"HSET", k.hone, "f", "v"},
		{"SADD", k.hone + ":s", "m"},
		{"SET", k.m1, "one"},
		{"SET", k.m2, "two"},
	} {
		if _, err := c3.Do(ctx, setup...); err != nil {
			t.Fatalf("setup %v: %v", setup[:2], err)
		}
	}

	show := func(s string, err error) string {
		if err != nil {
			return "ERR"
		}
		return s
	}
	for _, r := range dfRows(k) {
		t.Run(r.name, func(t *testing.T) {
			reply, err := c3.Do(ctx, r.args...)
			if err != nil {
				t.Fatalf("Do(%v) on RESP3: %v", r.args[0], err)
			}
			typeName := reply.Type.String()
			// check compares one helper across the protocols. named is true
			// when a RESP3 error must name the reply type.
			check := func(helper, pinned string, named bool, got2, got3 string, err3 error) {
				t.Helper()
				if pinned != "" && got2 != pinned {
					t.Errorf("%s on RESP2 = %q, want %q", helper, got2, pinned)
				}
				if got3 != got2 {
					t.Errorf("%s on RESP3 = %q (%v), on RESP2 = %q", helper, got3, err3, got2)
				}
				if err3 != nil && named && !strings.Contains(err3.Error(), typeName) {
					t.Errorf("%s on RESP3: error %q does not name the reply type %q", helper, err3, typeName)
				}
			}

			s2, e2 := c2.DoString(ctx, r.args...)
			s3, e3 := c3.DoString(ctx, r.args...)
			check("DoString", r.str, true, show(s2, e2), show(s3, e3), e3)

			n2, e2 := c2.DoInt(ctx, r.args...)
			n3, e3 := c3.DoInt(ctx, r.args...)
			// A bulk string that is not a number is a strconv error, which
			// names no reply type.
			check("DoInt", r.integer, reply.Type != celredis.TyBulk && reply.Type != celredis.TySimple,
				show(strconv.FormatInt(n2, 10), e2), show(strconv.FormatInt(n3, 10), e3), e3)

			b2, e2 := c2.DoBool(ctx, r.args...)
			b3, e3 := c3.DoBool(ctx, r.args...)
			check("DoBool", "", false, show(strconv.FormatBool(b2), e2), show(strconv.FormatBool(b3), e3), e3)

			l2, e2 := c2.DoSlice(ctx, r.args...)
			l3, e3 := c3.DoSlice(ctx, r.args...)
			check("DoSlice", r.slice, true, show(strings.Join(l2, "|"), e2), show(strings.Join(l3, "|"), e3), e3)
			if e2 == nil && e3 == nil && len(l2) == len(l3) {
				// The hole: an element the server sent must not read as "".
				for i, s := range l3 {
					if s == "" && l2[i] != "" {
						t.Errorf("DoSlice on RESP3: element %d is empty, RESP2 has %q", i, l2[i])
					}
				}
			}
		})
	}
}
