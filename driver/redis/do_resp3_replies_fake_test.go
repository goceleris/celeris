package redis

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// DoString, DoInt and DoSlice on a reply that is not a bulk string: an
// element of an array that is an integer, a double, a boolean, a big number,
// a map, a set or a nested array, or a scalar that is a double, a boolean, a
// big number or a map. The server here is in process and sends the RESP3
// bytes (and, where RESP2 has an equivalent, the RESP2 bytes), so the tests
// run in every job and need no Redis.
//
// A helper may return the value or an error that names the type it met. It
// must not return an empty string (or another wrong value) and no error.

// fakeReplyRow is one command and the reply a server sends for it.
type fakeReplyRow struct {
	name   string
	args   []any
	helper string // "slice", "string" or "int"
	resp3  string // the RESP3 bytes
	resp2  string // the RESP2 bytes; empty when RESP2 has no equivalent
	// want is the value the helper returns ("a|1" for a slice joined with
	// "|"), or "ERR:<type>" for an error that names the type.
	want string
}

func fakeReplyRows() []fakeReplyRow {
	const (
		pairs3  = "*2\r\n*2\r\n$1\r\na\r\n,1.5\r\n*2\r\n$1\r\nb\r\n,2\r\n"
		pairs2  = "*4\r\n$1\r\na\r\n$3\r\n1.5\r\n$1\r\nb\r\n$1\r\n2\r\n"
		nested2 = "*2\r\n*2\r\n$1\r\na\r\n$3\r\n1.5\r\n*2\r\n$1\r\nb\r\n$1\r\n2\r\n"
	)
	return []fakeReplyRow{
		{name: "slice_int_double_bool_bigint_bulk", args: []any{"XSLICE1"}, helper: "slice",
			resp3: "*5\r\n:7\r\n,2.5\r\n#t\r\n(123456789012345678901234567890\r\n$1\r\nx\r\n",
			resp2: "*5\r\n:7\r\n$3\r\n2.5\r\n:1\r\n$30\r\n123456789012345678901234567890\r\n$1\r\nx\r\n",
			want:  "7|2.5|1|123456789012345678901234567890|x"},
		{name: "slice_bool_false_and_negative_int", args: []any{"XSLICE2"}, helper: "slice",
			resp3: "*2\r\n#f\r\n:-3\r\n", resp2: "*2\r\n:0\r\n:-3\r\n", want: "0|-3"},
		{name: "slice_inf", args: []any{"XSLICE3"}, helper: "slice",
			resp3: "*2\r\n,inf\r\n,-inf\r\n", resp2: "*2\r\n$3\r\ninf\r\n$4\r\n-inf\r\n", want: "inf|-inf"},
		{name: "slice_null_element", args: []any{"XSLICE4"}, helper: "slice",
			resp3: "*3\r\n$1\r\na\r\n_\r\n$1\r\nb\r\n", resp2: "*3\r\n$1\r\na\r\n$-1\r\n$1\r\nb\r\n", want: "a||b"},
		{name: "slice_nested_array", args: []any{"XSLICE5"}, helper: "slice",
			resp3: "*2\r\n$1\r\nx\r\n*1\r\n$1\r\ny\r\n", resp2: "*2\r\n$1\r\nx\r\n*1\r\n$1\r\ny\r\n", want: "ERR:array"},
		{name: "slice_error_element", args: []any{"XSLICE6"}, helper: "slice",
			resp3: "*2\r\n$1\r\na\r\n-ERR boom\r\n", resp2: "*2\r\n$1\r\na\r\n-ERR boom\r\n", want: "ERR:error"},
		{name: "slice_map", args: []any{"XSLICE7"}, helper: "slice",
			resp3: "%2\r\n$1\r\na\r\n:1\r\n$1\r\nb\r\n,2.5\r\n", resp2: "*4\r\n$1\r\na\r\n:1\r\n$1\r\nb\r\n$3\r\n2.5\r\n", want: "a|1|b|2.5"},
		{name: "slice_set", args: []any{"XSLICE8"}, helper: "slice",
			resp3: "~2\r\n:1\r\n:2\r\n", resp2: "*2\r\n:1\r\n:2\r\n", want: "1|2"},
		{name: "slice_zrange_withscores", args: []any{"ZRANGE", "k9", 0, -1, "WITHSCORES"}, helper: "slice",
			resp3: pairs3, resp2: pairs2, want: "a|1.5|b|2"},
		{name: "slice_zrange_withscores_lowercase", args: []any{"zrange", "k10", "0", "-1", "withscores"}, helper: "slice",
			resp3: pairs3, resp2: pairs2, want: "a|1.5|b|2"},
		{name: "slice_hrandfield_withvalues", args: []any{"HRANDFIELD", "k11", 2, "WITHVALUES"}, helper: "slice",
			resp3: "*1\r\n*2\r\n$1\r\nf\r\n$1\r\nv\r\n", resp2: "*2\r\n$1\r\nf\r\n$1\r\nv\r\n", want: "f|v"},
		{name: "slice_pairs_without_an_option", args: []any{"XSLICE12"}, helper: "slice",
			resp3: pairs3, resp2: nested2, want: "ERR:array"},
		{name: "slice_withscores_but_not_pairs", args: []any{"ZRANGE", "k13", 0, -1, "WITHSCORES"}, helper: "slice",
			resp3: "*1\r\n*3\r\n$1\r\na\r\n$1\r\nb\r\n$1\r\nc\r\n", resp2: "", want: "ERR:array"},
		{name: "slice_withscores_pair_holding_an_array", args: []any{"ZRANGE", "k14", 0, -1, "WITHSCORES"}, helper: "slice",
			resp3: "*1\r\n*2\r\n$1\r\na\r\n*1\r\n$1\r\nb\r\n", resp2: "", want: "ERR:array"},
		{name: "string_double", args: []any{"XSTR1"}, helper: "string",
			resp3: ",3.5\r\n", resp2: "$3\r\n3.5\r\n", want: "3.5"},
		{name: "string_double_1234567", args: []any{"XSTR2"}, helper: "string",
			resp3: ",1234567\r\n", resp2: "$7\r\n1234567\r\n", want: "1234567"},
		{name: "string_bool", args: []any{"XSTR3"}, helper: "string",
			resp3: "#t\r\n", resp2: ":1\r\n", want: "1"},
		{name: "string_bigint", args: []any{"XSTR4"}, helper: "string",
			resp3: "(123456789012345678901234567890\r\n", resp2: "$30\r\n123456789012345678901234567890\r\n", want: "123456789012345678901234567890"},
		{name: "string_map", args: []any{"XSTR5"}, helper: "string",
			resp3: "%1\r\n$1\r\na\r\n:1\r\n", resp2: "", want: "ERR:map"},
		{name: "string_set", args: []any{"XSTR6"}, helper: "string",
			resp3: "~1\r\n:1\r\n", resp2: "", want: "ERR:set"},
		{name: "int_bool", args: []any{"XINT1"}, helper: "int",
			resp3: "#t\r\n", resp2: ":1\r\n", want: "1"},
		{name: "int_double_integral", args: []any{"XINT2"}, helper: "int",
			resp3: ",3\r\n", resp2: "$1\r\n3\r\n", want: "3"},
		{name: "int_double_fraction", args: []any{"XINT3"}, helper: "int",
			resp3: ",3.5\r\n", resp2: "", want: "ERR:double"},
		{name: "int_bigint_fits", args: []any{"XINT4"}, helper: "int",
			resp3: "(42\r\n", resp2: "$2\r\n42\r\n", want: "42"},
		{name: "int_bigint_too_large", args: []any{"XINT5"}, helper: "int",
			resp3: "(123456789012345678901234567890\r\n", resp2: "", want: "ERR:bigint"},
		{name: "int_map", args: []any{"XINT6"}, helper: "int",
			resp3: "%1\r\n$1\r\na\r\n:1\r\n", resp2: "", want: "ERR:map"},
	}
}

// fakeReplyClient is a client to an in-process server that answers each
// command of rows with the raw bytes of proto's column, and every other
// command (the handshake) as a healthy server.
func fakeReplyClient(t *testing.T, proto int, rows []fakeReplyRow) *Client {
	t.Helper()
	replies := make(map[string]string, len(rows))
	for _, r := range rows {
		raw := r.resp3
		if proto == 2 {
			raw = r.resp2
		}
		if raw == "" {
			continue
		}
		parts := make([]string, len(r.args))
		for i, a := range r.args {
			parts[i] = argify(a)
		}
		replies[strings.Join(parts, " ")] = raw
	}
	srv := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			return
		}
		if strings.EqualFold(cmd[0], "HELLO") {
			handleHELLO(w, 3)
			return
		}
		if raw, ok := replies[strings.Join(cmd, " ")]; ok {
			_, _ = w.WriteString(raw)
			return
		}
		writeSimple(w, "OK")
	})
	opts := []Option{WithPoolSize(1)}
	if proto == 2 {
		opts = append(opts, WithForceRESP2())
	}
	c, err := NewClient(srv.Addr(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.Proto() != proto {
		t.Fatalf("negotiated RESP%d, want RESP%d", c.Proto(), proto)
	}
	return c
}

// fakeReplyGot runs the row's helper.
func fakeReplyGot(ctx context.Context, c *Client, r fakeReplyRow) (string, error) {
	switch r.helper {
	case "slice":
		s, err := c.DoSlice(ctx, r.args...)
		return strings.Join(s, "|"), err
	case "string":
		return c.DoString(ctx, r.args...)
	case "int":
		n, err := c.DoInt(ctx, r.args...)
		return strconv.FormatInt(n, 10), err
	}
	panic("unknown helper " + r.helper)
}

// fakeTypeNames are the type names an error may name.
var fakeTypeNames = []string{"simple", "error", "int", "bulk", "array", "null", "bool", "double", "bigint", "bloberr", "verbatim", "set", "map", "attr", "push"}

func namesAType(err error) bool {
	for _, n := range fakeTypeNames {
		if strings.Contains(err.Error(), n) {
			return true
		}
	}
	return false
}

// checkFakeReply compares one helper result with want. When strict is false a
// value row also accepts an error that names a type (a decoder that refuses
// the element); only a wrong value with no error fails.
func checkFakeReply(t *testing.T, helper string, got string, err error, want string, strict bool) {
	t.Helper()
	if typ, isErr := strings.CutPrefix(want, "ERR:"); isErr {
		switch {
		case err == nil:
			t.Errorf("%s = %q and no error, want an error naming %q", helper, got, typ)
		case !strings.Contains(err.Error(), typ):
			t.Errorf("%s error %q does not name %q", helper, err, typ)
		}
		return
	}
	switch {
	case err == nil && got != want:
		t.Errorf("%s = %q and no error, want %q", helper, got, want)
	case err != nil && strict:
		t.Errorf("%s error %q, want %q", helper, err, want)
	case err != nil && !namesAType(err):
		t.Errorf("%s error %q names no type, want %q or an error that names the type", helper, err, want)
	}
}

// TestDoHelpersNoSilentWrongValue: on a RESP3 connection (the default) a
// helper returns the value or an error that names the type, never an empty
// string with no error. The one outcome that fails is a wrong value and a nil
// error.
func TestDoHelpersNoSilentWrongValue(t *testing.T) {
	rows := fakeReplyRows()
	c := fakeReplyClient(t, 3, rows)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, err := fakeReplyGot(ctx, c, r)
			checkFakeReply(t, r.helper, got, err, r.want, false)
		})
	}
}

// TestDoHelpersRESP3SameAsRESP2: the same helper returns the same value on a
// RESP3 connection and on a RESP2 connection, where RESP2 has an equivalent
// reply, and the value is the expected one.
func TestDoHelpersRESP3SameAsRESP2(t *testing.T) {
	rows := fakeReplyRows()
	c3 := fakeReplyClient(t, 3, rows)
	c2 := fakeReplyClient(t, 2, rows)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got3, err3 := fakeReplyGot(ctx, c3, r)
			checkFakeReply(t, r.helper+" on RESP3", got3, err3, r.want, true)
			if r.resp2 == "" {
				return
			}
			got2, err2 := fakeReplyGot(ctx, c2, r)
			checkFakeReply(t, r.helper+" on RESP2", got2, err2, r.want, true)
		})
	}
}
