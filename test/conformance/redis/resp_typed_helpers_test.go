//go:build redis

package redis_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// TestRESPDoHelpersAcrossProtocols: DoString, DoInt, DoBool and DoSlice read the same
// reply the same way on RESP3 and on RESP2. Each row is a script whose reply
// is one RESP3 type; the expectation is what RESP2 (the server's own
// downgrade: bulk, integer or flat array) gives through the same helper, and
// RESP3 must agree. RESP3-only types have no ordinary command, so they come
// from EVAL with redis.setresp(3). "ERR" means the helper returns an error.
func TestRESPDoHelpersAcrossProtocols(t *testing.T) {
	const set3 = "redis.setresp(3); "
	type row struct {
		name, script                 string
		str, integer, boolean, slice string // "ERR" or the expected rendering; slice is joined with "|"
	}
	rows := []row{
		{name: "bulk", script: "return 'bulk'", str: "bulk", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "integer", script: "return 7", str: "7", integer: "7", boolean: "true", slice: "ERR"},
		{name: "status", script: "return redis.status_reply('FINE')", str: "FINE", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "double", script: set3 + "return {double=3.5}", str: "3.5", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "double_inf", script: set3 + "return {double=math.huge}", str: "inf", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "double_neg_inf", script: set3 + "return {double=-math.huge}", str: "-inf", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "bool_true", script: set3 + "return true", str: "1", integer: "1", boolean: "true", slice: "ERR"},
		{name: "bool_false", script: set3 + "return false", str: "0", integer: "0", boolean: "false", slice: "ERR"},
		{name: "big_number", script: set3 + "return {big_number='123456789012345678901234567890'}", str: "123456789012345678901234567890", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "verbatim", script: set3 + "return {verbatim_string={string='hello', format='txt'}}", str: "hello", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "set", script: set3 + "return {set={a=true}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "a"},
		{name: "array_of_bulk", script: "return {'x','y'}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "x|y"},
		{name: "array_of_int", script: "return {1,2}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "1|2"},
		{name: "array_of_double", script: set3 + "return {{double=1.5},{double=2}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "1.5|2"},
		{name: "array_with_nested_array", script: "return {'x',{'y'}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "ERR"},
		{name: "map_flat_on_resp2", script: set3 + "return {map={a=1}}", str: "ERR", integer: "ERR", boolean: "ERR", slice: "a|1"},
	}
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
			ctx := ctxWithTimeout(t, 20*time.Second)
			show := func(s string, err error) string {
				if err != nil {
					return "ERR"
				}
				return s
			}
			for _, r := range rows {
				t.Run(r.name, func(t *testing.T) {
					args := []any{"EVAL", r.script, 0}
					s, err := c.DoString(ctx, args...)
					if got := show(s, err); got != r.str {
						t.Errorf("DoString = %q (%v), want %q", got, err, r.str)
					}
					n, err := c.DoInt(ctx, args...)
					if got := show(strconv.FormatInt(n, 10), err); got != r.integer {
						t.Errorf("DoInt = %q (%v), want %q", got, err, r.integer)
					}
					b, err := c.DoBool(ctx, args...)
					if got := show(strconv.FormatBool(b), err); got != r.boolean {
						t.Errorf("DoBool = %q (%v), want %q", got, err, r.boolean)
					}
					sl, err := c.DoSlice(ctx, args...)
					if got := show(strings.Join(sl, "|"), err); got != r.slice {
						t.Errorf("DoSlice = %q (%v), want %q", got, err, r.slice)
					}
				})
			}
		})
	}
}
