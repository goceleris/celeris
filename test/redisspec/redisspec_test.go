//go:build redisspec

package redisspec

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// ---------------------------------------------------------------------------
// Section 1: RESP2 Types
// ---------------------------------------------------------------------------

func TestRESP2_SimpleString(t *testing.T) {
	c := dialRedis(t)
	c.Send("PING")
	c.ExpectSimple(t, "PONG")
}

func TestRESP2_Error(t *testing.T) {
	c := dialRedis(t)
	// ERR is the standard error prefix for unknown commands.
	c.Send("FAKECOMMAND_RESP2_ERR")
	c.ExpectError(t, "ERR")
}

func TestRESP2_Integer(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "int")
	cleanupKey(t, c, k)

	c.Send("SET", k, "10")
	c.ExpectOK(t)

	c.Send("INCR", k)
	c.ExpectInt(t, 11)
}

func TestRESP2_BulkString(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "bulk")
	cleanupKey(t, c, k)

	c.Send("SET", k, "hello")
	c.ExpectOK(t)

	c.Send("GET", k)
	c.ExpectBulk(t, "hello")
}

func TestRESP2_NullBulk(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "nullbulk")
	// Key does not exist, GET returns null bulk.
	c.Send("GET", k)
	c.ExpectNull(t)
}

func TestRESP2_Array(t *testing.T) {
	c := dialRedis(t)
	k1 := uniqueKey(t, "arr1")
	k2 := uniqueKey(t, "arr2")
	k3 := uniqueKey(t, "arr3")
	cleanupKey(t, c, k1, k2, k3)

	c.Send("SET", k1, "a")
	c.ExpectOK(t)
	c.Send("SET", k2, "b")
	c.ExpectOK(t)
	c.Send("SET", k3, "c")
	c.ExpectOK(t)

	c.Send("MGET", k1, k2, k3)
	arr := c.ExpectArray(t, 3)
	for i, want := range []string{"a", "b", "c"} {
		if arr[i].Type != protocol.TyBulk || string(arr[i].Str) != want {
			t.Fatalf("MGET[%d]: got %v %q, want bulk %q", i, arr[i].Type, arr[i].Str, want)
		}
	}
}

func TestRESP2_NullArray(t *testing.T) {
	c := dialRedis(t)
	// BLPOP with 0.1s timeout on a non-existent list returns null array.
	k := uniqueKey(t, "nullarr")
	c.Send("BLPOP", k, "0.1")
	v := c.ReadValue(t)
	if v.Type != protocol.TyNull {
		// Some servers may return an empty array instead of null.
		if v.Type == protocol.TyArray && len(v.Array) == 0 {
			return
		}
		t.Fatalf("expected null (or empty array), got %v", v.Type)
	}
}

func TestRESP2_EmptyArray(t *testing.T) {
	c := dialRedis(t)
	// KEYS with a pattern that matches nothing returns *0.
	c.Send("KEYS", "redisspec:nonexistent:pattern:zzzzz:*")
	arr := c.ExpectArray(t, 0)
	if len(arr) != 0 {
		t.Fatalf("expected empty array, got %d elements", len(arr))
	}
}

func TestRESP2_NestedArray(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "nested")
	cleanupKey(t, c, k)

	// MULTI/EXEC returns an array of results.
	c.Send("MULTI")
	c.ExpectOK(t)

	c.Send("SET", k, "val")
	c.ExpectQueued(t)

	c.Send("GET", k)
	c.ExpectQueued(t)

	c.Send("EXEC")
	v := c.ReadValue(t)
	if v.Type != protocol.TyArray {
		t.Fatalf("EXEC: expected array, got %v", v.Type)
	}
	if len(v.Array) != 2 {
		t.Fatalf("EXEC: expected 2 results, got %d", len(v.Array))
	}
	// First result: SET -> OK
	if v.Array[0].Type != protocol.TySimple || string(v.Array[0].Str) != "OK" {
		t.Fatalf("EXEC[0]: %v %q", v.Array[0].Type, v.Array[0].Str)
	}
	// Second result: GET -> bulk "val"
	if v.Array[1].Type != protocol.TyBulk || string(v.Array[1].Str) != "val" {
		t.Fatalf("EXEC[1]: %v %q", v.Array[1].Type, v.Array[1].Str)
	}
}

func TestRESP2_EmptyBulk(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "emptybulk")
	cleanupKey(t, c, k)

	c.Send("SET", k, "")
	c.ExpectOK(t)

	// GET should return $0\r\n\r\n (zero-length bulk, NOT null).
	c.Send("GET", k)
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk {
		t.Fatalf("expected bulk string, got %v", v.Type)
	}
	if len(v.Str) != 0 {
		t.Fatalf("expected empty bulk, got %q", v.Str)
	}
}

// ---------------------------------------------------------------------------
// Section 2: RESP3 Types (requires Redis 6.0+)
// ---------------------------------------------------------------------------

func TestRESP3_HELLO(t *testing.T) {
	c := dialRedis(t)
	c.Send("HELLO", "3")
	v := c.ReadValue(t)
	if v.Type == protocol.TyError {
		t.Skipf("server does not support HELLO 3: %s", v.Str)
	}
	// RESP3 HELLO returns a map with server info.
	if v.Type != protocol.TyMap && v.Type != protocol.TyArray {
		t.Fatalf("HELLO 3: expected map or array, got %v", v.Type)
	}
}

func TestRESP3_Null(t *testing.T) {
	c := dialRedisRESP3(t)
	k := uniqueKey(t, "resp3null")
	c.Send("GET", k)
	v := c.ReadValue(t)
	// In RESP3, null is _\r\n (TyNull).
	if v.Type != protocol.TyNull {
		t.Fatalf("expected RESP3 null, got %v", v.Type)
	}
}

func TestRESP3_Boolean(t *testing.T) {
	c := dialRedisRESP3(t)
	k := uniqueKey(t, "resp3bool")
	cleanupKey(t, c, k)

	c.Send("SADD", k, "member1")
	_ = c.ReadValue(t) // :1

	c.Send("SISMEMBER", k, "member1")
	v := c.ReadValue(t)
	// RESP3 returns boolean for SISMEMBER.
	if v.Type == protocol.TyBool {
		if !v.Bool {
			t.Fatal("SISMEMBER: expected true, got false")
		}
	} else if v.Type == protocol.TyInt {
		// Fallback: some servers still return integer.
		if v.Int != 1 {
			t.Fatalf("SISMEMBER: expected 1, got %d", v.Int)
		}
	} else {
		t.Fatalf("SISMEMBER: unexpected type %v", v.Type)
	}

	c.Send("SISMEMBER", k, "nonexistent")
	v = c.ReadValue(t)
	if v.Type == protocol.TyBool {
		if v.Bool {
			t.Fatal("SISMEMBER: expected false, got true")
		}
	} else if v.Type == protocol.TyInt {
		if v.Int != 0 {
			t.Fatalf("SISMEMBER: expected 0, got %d", v.Int)
		}
	}
}

func TestRESP3_Double(t *testing.T) {
	c := dialRedisRESP3(t)
	k := uniqueKey(t, "resp3dbl")
	cleanupKey(t, c, k)

	c.Send("ZADD", k, "3.14", "member1")
	_ = c.ReadValue(t) // :1

	c.Send("ZSCORE", k, "member1")
	v := c.ReadValue(t)
	if v.Type == protocol.TyDouble {
		if v.Float < 3.13 || v.Float > 3.15 {
			t.Fatalf("ZSCORE: expected ~3.14, got %f", v.Float)
		}
	} else if v.Type == protocol.TyBulk {
		// RESP2 fallback.
		if string(v.Str) != "3.14" {
			t.Fatalf("ZSCORE: expected 3.14, got %q", v.Str)
		}
	} else {
		t.Fatalf("ZSCORE: unexpected type %v", v.Type)
	}
}

func TestRESP3_BigNumber(t *testing.T) {
	c := dialRedisRESP3(t)
	// DEBUG SET-ACTIVE-EXPIRE is one of few commands returning big numbers.
	// This test validates the parser handles ( prefix if seen.
	// Most Redis commands don't return big numbers, so we just verify HELLO
	// negotiation worked and the connection is functional.
	c.Send("PING")
	v := c.ReadValue(t)
	if v.Type != protocol.TySimple || string(v.Str) != "PONG" {
		t.Fatalf("PING after HELLO 3: %v %q", v.Type, v.Str)
	}
}

func TestRESP3_BlobError(t *testing.T) {
	// Blob errors (!) are rarely emitted by standard commands. We verify
	// our parser handles them via direct injection in fuzz_test.go. Here
	// we just verify the RESP3 connection handles normal errors.
	c := dialRedisRESP3(t)
	c.Send("FAKECOMMAND_RESP3_BLOBERR")
	v := c.ReadValue(t)
	// Server may return simple error (-) or blob error (!).
	if v.Type != protocol.TyError && v.Type != protocol.TyBlobErr {
		t.Fatalf("expected error type, got %v", v.Type)
	}
}

func TestRESP3_VerbatimString(t *testing.T) {
	// LOLWUT in RESP3 mode may return a verbatim string.
	c := dialRedisRESP3(t)
	c.Send("LOLWUT")
	v := c.ReadValue(t)
	// Accept verbatim (=) or bulk ($) depending on server.
	if v.Type != protocol.TyVerbatim && v.Type != protocol.TyBulk {
		t.Fatalf("LOLWUT: expected verbatim or bulk, got %v", v.Type)
	}
}

func TestRESP3_Map(t *testing.T) {
	c := dialRedisRESP3(t)
	k := uniqueKey(t, "resp3map")
	cleanupKey(t, c, k)

	c.Send("HSET", k, "field1", "val1", "field2", "val2")
	_ = c.ReadValue(t) // :2

	c.Send("HGETALL", k)
	v := c.ReadValue(t)
	if v.Type == protocol.TyMap {
		if len(v.Map) != 2 {
			t.Fatalf("HGETALL: expected 2 pairs, got %d", len(v.Map))
		}
	} else if v.Type == protocol.TyArray {
		// RESP2 fallback: flat array.
		if len(v.Array) != 4 {
			t.Fatalf("HGETALL: expected 4 elements, got %d", len(v.Array))
		}
	} else {
		t.Fatalf("HGETALL: unexpected type %v", v.Type)
	}
}

func TestRESP3_Set(t *testing.T) {
	c := dialRedisRESP3(t)
	k := uniqueKey(t, "resp3set")
	cleanupKey(t, c, k)

	c.Send("SADD", k, "a", "b", "c")
	_ = c.ReadValue(t) // :3

	c.Send("SMEMBERS", k)
	v := c.ReadValue(t)
	if v.Type == protocol.TySet {
		if len(v.Array) != 3 {
			t.Fatalf("SMEMBERS: expected 3 elements, got %d", len(v.Array))
		}
	} else if v.Type == protocol.TyArray {
		if len(v.Array) != 3 {
			t.Fatalf("SMEMBERS: expected 3 elements, got %d", len(v.Array))
		}
	} else {
		t.Fatalf("SMEMBERS: unexpected type %v", v.Type)
	}
}

func TestRESP3_Push(t *testing.T) {
	c := dialRedisRESP3(t)
	ch := uniqueKey(t, "resp3push")

	c.Send("SUBSCRIBE", ch)
	v := c.ReadValue(t)
	// In RESP3, subscription acks are push frames (>).
	if v.Type != protocol.TyPush && v.Type != protocol.TyArray {
		t.Fatalf("SUBSCRIBE ack: expected push or array, got %v", v.Type)
	}

	// Unsubscribe to restore normal mode.
	c.Send("UNSUBSCRIBE", ch)
	_ = c.ReadValue(t)
}

func TestRESP3_Attribute(t *testing.T) {
	// Attribute frames (|) are optional server annotations. Most servers
	// don't emit them. We verify our parser handles them (tested in unit
	// tests and fuzz). Here we just verify RESP3 mode is functional.
	c := dialRedisRESP3(t)
	c.Send("PING")
	c.ExpectSimple(t, "PONG")
}

// ---------------------------------------------------------------------------
// Section 3: Command Protocol
// ---------------------------------------------------------------------------

func TestCmd_InlineFormat(t *testing.T) {
	c := dialRedis(t)
	// Send as raw inline (not RESP array).
	c.SendRaw([]byte("PING\r\n"))
	c.ExpectSimple(t, "PONG")
}

func TestCmd_MultiBulk(t *testing.T) {
	c := dialRedis(t)
	// Standard *N\r\n$len\r\narg\r\n... format.
	c.Send("PING")
	c.ExpectSimple(t, "PONG")
}

func TestCmd_Pipeline(t *testing.T) {
	c := dialRedis(t)
	const n = 100
	// Send all 100 PINGs without reading.
	w := protocol.NewWriter()
	for i := 0; i < n; i++ {
		w.AppendCommand1("PING")
	}
	c.SendRaw(w.Bytes())

	// Read all 100 PONGs in order.
	for i := 0; i < n; i++ {
		v := c.ReadValue(t)
		if v.Type != protocol.TySimple || string(v.Str) != "PONG" {
			t.Fatalf("pipeline[%d]: %v %q", i, v.Type, v.Str)
		}
	}
}

func TestCmd_PipelineError(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "piperr")
	cleanupKey(t, c, k)

	w := protocol.NewWriter()
	w.AppendCommand3("SET", k, "hello")
	w.AppendCommand("FAKECOMMAND_PIPELINE")
	w.AppendCommand2("GET", k)
	c.SendRaw(w.Bytes())

	// SET -> OK
	c.ExpectOK(t)
	// FAKECOMMAND -> error
	c.ExpectError(t, "ERR")
	// GET -> bulk "hello" (unaffected by previous error)
	c.ExpectBulk(t, "hello")
}

func TestCmd_MaxArgs(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "maxargs")
	cleanupKey(t, c, k)

	// Build SADD with 1000 members.
	args := make([]string, 1002)
	args[0] = "SADD"
	args[1] = k
	for i := 2; i < 1002; i++ {
		args[i] = fmt.Sprintf("member%d", i-2)
	}
	c.Send(args...)
	v := c.ReadValue(t)
	if v.Type != protocol.TyInt || v.Int != 1000 {
		t.Fatalf("SADD 1000: expected :1000, got %v %d", v.Type, v.Int)
	}
}

func TestCmd_LargeBulk(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "largebulk")
	cleanupKey(t, c, k)

	// 10MB value.
	val := strings.Repeat("X", 10*1024*1024)
	c.Send("SET", k, val)
	c.ExpectOK(t)

	c.Send("GET", k)
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk {
		t.Fatalf("expected bulk, got %v", v.Type)
	}
	if len(v.Str) != len(val) {
		t.Fatalf("bulk length %d, want %d", len(v.Str), len(val))
	}
}

func TestCmd_EmptyCommand(t *testing.T) {
	c := dialRedis(t)
	// Send *0\r\n (empty array command).
	c.SendRaw([]byte("*0\r\n"))
	// Server should return an error or close the connection.
	v, ok := c.ReadValueTimeout(t, 2*time.Second)
	if ok && v.Type != protocol.TyError {
		// Some servers just ignore *0; verify the connection still works.
		c.Send("PING")
		c.ExpectSimple(t, "PONG")
	}
}

func TestCmd_UnknownCommand(t *testing.T) {
	c := dialRedis(t)
	c.Send("FAKECOMMAND_UNKNOWN")
	c.ExpectError(t, "ERR")
}

// ---------------------------------------------------------------------------
// Section 4: AUTH + SELECT
// ---------------------------------------------------------------------------

func TestAuth_NoAuth(t *testing.T) {
	// If server doesn't require auth, commands work immediately.
	c := dialRedisNoAuth(t)
	c.Send("PING")
	v := c.ReadValue(t)
	if v.Type == protocol.TyError && strings.HasPrefix(string(v.Str), "NOAUTH") {
		t.Skip("server requires authentication")
	}
	if v.Type != protocol.TySimple || string(v.Str) != "PONG" {
		t.Fatalf("expected PONG, got %v %q", v.Type, v.Str)
	}
}

func TestAuth_RequiredButMissing(t *testing.T) {
	pw := passwordFromEnv(t)
	_ = pw
	c := dialRedisNoAuth(t)
	c.Send("PING")
	v := c.ReadValue(t)
	if v.Type != protocol.TyError {
		t.Skip("server does not require auth (no NOAUTH error)")
	}
	if !strings.HasPrefix(string(v.Str), "NOAUTH") {
		t.Fatalf("expected NOAUTH, got %q", v.Str)
	}
}

func TestAuth_Correct(t *testing.T) {
	pw := passwordFromEnv(t)
	c := dialRedisNoAuth(t)
	c.Send("AUTH", pw)
	c.ExpectOK(t)
}

func TestAuth_Wrong(t *testing.T) {
	_ = passwordFromEnv(t)
	c := dialRedisNoAuth(t)
	c.Send("AUTH", "definitelywrongpassword12345")
	v := c.ReadValue(t)
	if v.Type != protocol.TyError {
		t.Fatalf("expected error, got %v", v.Type)
	}
	errStr := string(v.Str)
	if !strings.HasPrefix(errStr, "WRONGPASS") && !strings.HasPrefix(errStr, "ERR") {
		t.Fatalf("expected WRONGPASS or ERR, got %q", errStr)
	}
}

func TestAuth_ACL(t *testing.T) {
	pw := passwordFromEnv(t)
	c := dialRedisNoAuth(t)
	// Redis 6+ ACL format: AUTH user password.
	c.Send("AUTH", "default", pw)
	v := c.ReadValue(t)
	if v.Type == protocol.TyError {
		errStr := string(v.Str)
		if strings.Contains(errStr, "wrong number of arguments") {
			t.Skip("server does not support ACL AUTH")
		}
		t.Fatalf("AUTH default: %s", errStr)
	}
	if v.Type != protocol.TySimple || string(v.Str) != "OK" {
		t.Fatalf("expected OK, got %v %q", v.Type, v.Str)
	}
}

func TestSelect_ValidDB(t *testing.T) {
	c := dialRedis(t)
	c.Send("SELECT", "0")
	c.ExpectOK(t)
	c.Send("SELECT", "1")
	c.ExpectOK(t)
}

func TestSelect_InvalidDB(t *testing.T) {
	c := dialRedis(t)
	c.Send("SELECT", "99999")
	c.ExpectError(t, "ERR")
}

// ---------------------------------------------------------------------------
// Section 5: Pub/Sub Protocol
// ---------------------------------------------------------------------------

func TestPubSub_Subscribe(t *testing.T) {
	c := dialRedis(t)
	ch := uniqueKey(t, "pubsub")

	c.Send("SUBSCRIBE", ch)
	v := c.ReadValue(t)
	// Expect ["subscribe", ch, 1].
	assertSubscribeAck(t, v, "subscribe", ch, 1)

	c.Send("UNSUBSCRIBE", ch)
	_ = c.ReadValue(t)
}

func TestPubSub_Message(t *testing.T) {
	sub := dialRedis(t)
	pub := dialRedis(t)
	ch := uniqueKey(t, "pubmsg")

	sub.Send("SUBSCRIBE", ch)
	v := sub.ReadValue(t)
	assertSubscribeAck(t, v, "subscribe", ch, 1)

	pub.Send("PUBLISH", ch, "hello")
	pv := pub.ReadValue(t)
	if pv.Type != protocol.TyInt || pv.Int < 1 {
		t.Fatalf("PUBLISH: expected >= :1 receivers, got %v %d", pv.Type, pv.Int)
	}

	// Read message on subscriber.
	msg := sub.ReadValue(t)
	assertPubSubMessage(t, msg, "message", ch, "hello")

	sub.Send("UNSUBSCRIBE", ch)
	_ = sub.ReadValue(t)
}

func TestPubSub_PatternSubscribe(t *testing.T) {
	sub := dialRedis(t)
	pub := dialRedis(t)
	prefix := uniqueKey(t, "psub")
	pattern := prefix + ":*"
	channel := prefix + ":test"

	sub.Send("PSUBSCRIBE", pattern)
	v := sub.ReadValue(t)
	assertSubscribeAck(t, v, "psubscribe", pattern, 1)

	pub.Send("PUBLISH", channel, "data")
	pv := pub.ReadValue(t)
	if pv.Type != protocol.TyInt || pv.Int < 1 {
		t.Fatalf("PUBLISH: expected >= :1, got %v %d", pv.Type, pv.Int)
	}

	// Read pmessage: ["pmessage", pattern, channel, "data"].
	msg := sub.ReadValue(t)
	assertPubSubPMessage(t, msg, pattern, channel, "data")

	sub.Send("PUNSUBSCRIBE", pattern)
	_ = sub.ReadValue(t)
}

func TestPubSub_Unsubscribe(t *testing.T) {
	c := dialRedis(t)
	ch := uniqueKey(t, "unsub")

	c.Send("SUBSCRIBE", ch)
	_ = c.ReadValue(t) // subscribe ack

	c.Send("UNSUBSCRIBE", ch)
	v := c.ReadValue(t)
	assertSubscribeAck(t, v, "unsubscribe", ch, 0)
}

func TestPubSub_MultiChannel(t *testing.T) {
	c := dialRedis(t)
	ch1 := uniqueKey(t, "multi1")
	ch2 := uniqueKey(t, "multi2")
	ch3 := uniqueKey(t, "multi3")

	c.Send("SUBSCRIBE", ch1, ch2, ch3)
	for i, ch := range []string{ch1, ch2, ch3} {
		v := c.ReadValue(t)
		assertSubscribeAck(t, v, "subscribe", ch, int64(i+1))
	}

	c.Send("UNSUBSCRIBE")
	for i := 0; i < 3; i++ {
		_ = c.ReadValue(t)
	}
}

func TestPubSub_PingInPubSub(t *testing.T) {
	c := dialRedis(t)
	ch := uniqueKey(t, "pingps")

	c.Send("SUBSCRIBE", ch)
	_ = c.ReadValue(t)

	// PING is allowed during subscription.
	c.Send("PING")
	v := c.ReadValue(t)
	// In subscription mode, PING returns as a message array or push.
	arr := extractArray(v)
	if len(arr) >= 2 {
		kind := strings.ToLower(string(arr[0].Str))
		if kind != "pong" && kind != "ping" {
			t.Fatalf("expected pong/ping frame, got %q", kind)
		}
	} else if v.Type == protocol.TySimple && string(v.Str) == "PONG" {
		// Some servers return simple PONG even in sub mode.
	} else {
		t.Fatalf("unexpected PING response in sub mode: %v", v.Type)
	}

	c.Send("UNSUBSCRIBE", ch)
	_ = c.ReadValue(t)
}

func TestPubSub_NonSubCommand(t *testing.T) {
	c := dialRedis(t)
	ch := uniqueKey(t, "nonsub")

	c.Send("SUBSCRIBE", ch)
	_ = c.ReadValue(t)

	// GET is not allowed in subscription mode.
	c.Send("GET", "somekey")
	v := c.ReadValue(t)
	// Server should return an error.
	if v.Type == protocol.TyError || v.Type == protocol.TyBlobErr {
		// Good - expected.
	} else {
		// Some RESP3 push-mode servers may wrap this differently.
		arr := extractArray(v)
		if len(arr) > 0 && arr[0].Type == protocol.TyError {
			// Also acceptable.
		} else {
			t.Logf("NOTE: server allowed GET during subscription: %v", v.Type)
		}
	}

	c.Send("UNSUBSCRIBE", ch)
	_ = c.ReadValue(t)
}

func TestPubSub_UnsubscribeAll(t *testing.T) {
	c := dialRedis(t)
	ch1 := uniqueKey(t, "unall1")
	ch2 := uniqueKey(t, "unall2")

	c.Send("SUBSCRIBE", ch1, ch2)
	_ = c.ReadValue(t)
	_ = c.ReadValue(t)

	// UNSUBSCRIBE with no args unsubscribes from all channels.
	c.Send("UNSUBSCRIBE")
	// Should get two unsubscribe acks.
	for i := 0; i < 2; i++ {
		v := c.ReadValue(t)
		arr := extractArray(v)
		if len(arr) >= 1 {
			kind := strings.ToLower(string(arr[0].Str))
			if kind != "unsubscribe" {
				t.Fatalf("expected unsubscribe ack, got %q", kind)
			}
		}
	}
}

func TestPubSub_RESP3Push(t *testing.T) {
	c := dialRedisRESP3(t)
	ch := uniqueKey(t, "r3push")

	c.Send("SUBSCRIBE", ch)
	v := c.ReadValue(t)
	// In RESP3, push messages use > prefix.
	if v.Type != protocol.TyPush && v.Type != protocol.TyArray {
		t.Fatalf("RESP3 subscribe ack: expected push or array, got %v", v.Type)
	}

	c.Send("UNSUBSCRIBE", ch)
	_ = c.ReadValue(t)
}

// ---------------------------------------------------------------------------
// Section 6: Transaction Protocol (MULTI/EXEC)
// ---------------------------------------------------------------------------

func TestTx_MultiExec(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "tx")
	cleanupKey(t, c, k)

	c.Send("MULTI")
	c.ExpectOK(t)

	c.Send("SET", k, "txval")
	c.ExpectQueued(t)

	c.Send("GET", k)
	c.ExpectQueued(t)

	c.Send("EXEC")
	arr := c.ExpectArray(t, 2)
	if arr[0].Type != protocol.TySimple || string(arr[0].Str) != "OK" {
		t.Fatalf("EXEC[0]: %v %q", arr[0].Type, arr[0].Str)
	}
	if arr[1].Type != protocol.TyBulk || string(arr[1].Str) != "txval" {
		t.Fatalf("EXEC[1]: %v %q", arr[1].Type, arr[1].Str)
	}
}

func TestTx_Discard(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "txdisc")
	cleanupKey(t, c, k)

	c.Send("MULTI")
	c.ExpectOK(t)

	c.Send("SET", k, "discarded")
	c.ExpectQueued(t)

	c.Send("DISCARD")
	c.ExpectOK(t)

	// Key should not exist.
	c.Send("GET", k)
	c.ExpectNull(t)
}

func TestTx_EmptyExec(t *testing.T) {
	c := dialRedis(t)
	c.Send("MULTI")
	c.ExpectOK(t)

	c.Send("EXEC")
	arr := c.ExpectArray(t, 0)
	if len(arr) != 0 {
		t.Fatalf("expected empty EXEC result, got %d elements", len(arr))
	}
}

func TestTx_ErrorQueued(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "txeq")
	cleanupKey(t, c, k)

	// Set a string key, then try a list operation on it.
	c.Send("SET", k, "notalist")
	c.ExpectOK(t)

	c.Send("MULTI")
	c.ExpectOK(t)

	c.Send("LPUSH", k, "item")
	c.ExpectQueued(t)

	c.Send("EXEC")
	v := c.ReadValue(t)
	if v.Type != protocol.TyArray || len(v.Array) != 1 {
		t.Fatalf("EXEC: expected array of 1, got %v len=%d", v.Type, len(v.Array))
	}
	// The LPUSH should have failed with WRONGTYPE.
	if v.Array[0].Type != protocol.TyError {
		t.Fatalf("EXEC[0]: expected error, got %v", v.Array[0].Type)
	}
	if !strings.HasPrefix(string(v.Array[0].Str), "WRONGTYPE") {
		t.Fatalf("EXEC[0]: expected WRONGTYPE, got %q", v.Array[0].Str)
	}
}

func TestTx_ExecAbort(t *testing.T) {
	c := dialRedis(t)

	c.Send("MULTI")
	c.ExpectOK(t)

	// Send a command with the wrong number of arguments (syntax error).
	c.Send("SET")
	v := c.ReadValue(t)
	if v.Type != protocol.TyError {
		t.Fatalf("expected error for bad SET, got %v", v.Type)
	}

	c.Send("EXEC")
	v = c.ReadValue(t)
	if v.Type != protocol.TyError {
		// Some servers return -EXECABORT.
		t.Fatalf("expected EXEC error (-EXECABORT), got %v %q", v.Type, v.Str)
	}
	errStr := string(v.Str)
	if !strings.Contains(errStr, "EXECABORT") && !strings.Contains(errStr, "ERR") {
		t.Fatalf("expected EXECABORT or ERR, got %q", errStr)
	}
}

func TestTx_Watch(t *testing.T) {
	c1 := dialRedis(t)
	c2 := dialRedis(t)
	k := uniqueKey(t, "txwatch")
	cleanupKey(t, c1, k)

	c1.Send("SET", k, "original")
	c1.ExpectOK(t)

	c1.Send("WATCH", k)
	c1.ExpectOK(t)

	c1.Send("MULTI")
	c1.ExpectOK(t)

	c1.Send("GET", k)
	c1.ExpectQueued(t)

	// Concurrent modification on another connection.
	c2.Send("SET", k, "modified")
	c2.ExpectOK(t)

	// EXEC should return null (transaction aborted).
	c1.Send("EXEC")
	v := c1.ReadValue(t)
	if v.Type != protocol.TyNull {
		t.Fatalf("WATCH: expected null (aborted), got %v", v.Type)
	}
}

func TestTx_Unwatch(t *testing.T) {
	c1 := dialRedis(t)
	c2 := dialRedis(t)
	k := uniqueKey(t, "txunwatch")
	cleanupKey(t, c1, k)

	c1.Send("SET", k, "original")
	c1.ExpectOK(t)

	c1.Send("WATCH", k)
	c1.ExpectOK(t)

	c1.Send("UNWATCH")
	c1.ExpectOK(t)

	// Modify from second connection.
	c2.Send("SET", k, "modified")
	c2.ExpectOK(t)

	c1.Send("MULTI")
	c1.ExpectOK(t)

	c1.Send("GET", k)
	c1.ExpectQueued(t)

	// EXEC should succeed despite concurrent modification (UNWATCH cleared it).
	c1.Send("EXEC")
	arr := c1.ExpectArray(t, 1)
	if arr[0].Type != protocol.TyBulk || string(arr[0].Str) != "modified" {
		t.Fatalf("UNWATCH: EXEC[0] = %v %q, want modified", arr[0].Type, arr[0].Str)
	}
}

// ---------------------------------------------------------------------------
// Section 7: Wire Edge Cases
// ---------------------------------------------------------------------------

func TestWire_SplitReads(t *testing.T) {
	c := dialRedis(t)

	// Build a PING command and send it byte by byte.
	w := protocol.NewWriter()
	w.AppendCommand1("PING")
	data := w.Bytes()

	for _, b := range data {
		c.SendRaw([]byte{b})
		time.Sleep(time.Millisecond)
	}
	c.ExpectSimple(t, "PONG")
}

func TestWire_BackToBack(t *testing.T) {
	c := dialRedis(t)
	const n = 10000

	w := protocol.NewWriter()
	for i := 0; i < n; i++ {
		w.AppendCommand1("PING")
	}
	c.SendRaw(w.Bytes())

	for i := 0; i < n; i++ {
		v := c.ReadValue(t)
		if v.Type != protocol.TySimple || string(v.Str) != "PONG" {
			t.Fatalf("flood[%d]: %v %q", i, v.Type, v.Str)
		}
	}
}

func TestWire_BinaryKey(t *testing.T) {
	c := dialRedis(t)
	// Key with null bytes and CRLF.
	k := uniqueKey(t, "bin") + "\x00\r\nspecial"
	cleanupKey(t, c, k)

	val := "binary\x00value\r\n"
	c.Send("SET", k, val)
	c.ExpectOK(t)

	c.Send("GET", k)
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk {
		t.Fatalf("expected bulk, got %v", v.Type)
	}
	if !bytes.Equal(v.Str, []byte(val)) {
		t.Fatalf("binary round-trip failed: got %q, want %q", v.Str, val)
	}
}

func TestWire_MaxBulkSize(t *testing.T) {
	// Test approaching the server's proto-max-bulk-len (default 512MB).
	// We use a smaller value (1MB) to keep the test fast.
	c := dialRedis(t)
	k := uniqueKey(t, "maxbulk")
	cleanupKey(t, c, k)

	val := strings.Repeat("A", 1024*1024) // 1MB
	c.Send("SET", k, val)
	c.ExpectOK(t)

	c.Send("GET", k)
	v := c.ReadValue(t)
	if v.Type != protocol.TyBulk || len(v.Str) != len(val) {
		t.Fatalf("bulk size %d, want %d", len(v.Str), len(val))
	}
}

func TestWire_ConcurrentConns(t *testing.T) {
	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := dialRedis(t)
			k := uniqueKey(t, fmt.Sprintf("conc%d", id))
			val := strconv.Itoa(id)

			c.Send("SET", k, val)
			v := c.ReadValue(t)
			if v.Type == protocol.TyError {
				errs <- fmt.Errorf("conn %d SET: %s", id, v.Str)
				return
			}

			c.Send("GET", k)
			v = c.ReadValue(t)
			if v.Type != protocol.TyBulk || string(v.Str) != val {
				errs <- fmt.Errorf("conn %d GET: got %v %q, want %q", id, v.Type, v.Str, val)
				return
			}

			// Cleanup.
			c.Send("DEL", k)
			_ = c.ReadValue(t)
		}(i)
	}

	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestWire_ClientNameTracking(t *testing.T) {
	c := dialRedis(t)
	name := "redisspec-test-client"

	c.Send("CLIENT", "SETNAME", name)
	c.ExpectOK(t)

	c.Send("CLIENT", "GETNAME")
	c.ExpectBulk(t, name)
}

// ---------------------------------------------------------------------------
// helpers for pub/sub assertions
// ---------------------------------------------------------------------------

// extractArray returns the Array field from an array or push value.
func extractArray(v protocol.Value) []protocol.Value {
	if v.Type == protocol.TyArray || v.Type == protocol.TyPush {
		return v.Array
	}
	return nil
}

func assertSubscribeAck(t *testing.T, v protocol.Value, kind, channel string, count int64) {
	t.Helper()
	arr := extractArray(v)
	if arr == nil || len(arr) < 3 {
		t.Fatalf("subscribe ack: expected array/push of 3+, got %v len=%d", v.Type, len(arr))
	}
	if strings.ToLower(string(arr[0].Str)) != kind {
		t.Fatalf("subscribe ack[0]: expected %q, got %q", kind, arr[0].Str)
	}
	if string(arr[1].Str) != channel {
		t.Fatalf("subscribe ack[1]: expected %q, got %q", channel, arr[1].Str)
	}
	if arr[2].Int != count {
		t.Fatalf("subscribe ack[2]: expected %d, got %d", count, arr[2].Int)
	}
}

func assertPubSubMessage(t *testing.T, v protocol.Value, kind, channel, payload string) {
	t.Helper()
	arr := extractArray(v)
	if arr == nil || len(arr) < 3 {
		t.Fatalf("message: expected array/push of 3+, got %v len=%d", v.Type, len(arr))
	}
	if strings.ToLower(string(arr[0].Str)) != kind {
		t.Fatalf("message[0]: expected %q, got %q", kind, arr[0].Str)
	}
	if string(arr[1].Str) != channel {
		t.Fatalf("message[1]: expected %q, got %q", channel, arr[1].Str)
	}
	if string(arr[2].Str) != payload {
		t.Fatalf("message[2]: expected %q, got %q", payload, arr[2].Str)
	}
}

func assertPubSubPMessage(t *testing.T, v protocol.Value, pattern, channel, payload string) {
	t.Helper()
	arr := extractArray(v)
	if arr == nil || len(arr) < 4 {
		t.Fatalf("pmessage: expected array/push of 4+, got %v len=%d", v.Type, len(arr))
	}
	if strings.ToLower(string(arr[0].Str)) != "pmessage" {
		t.Fatalf("pmessage[0]: expected pmessage, got %q", arr[0].Str)
	}
	if string(arr[1].Str) != pattern {
		t.Fatalf("pmessage[1]: expected %q, got %q", pattern, arr[1].Str)
	}
	if string(arr[2].Str) != channel {
		t.Fatalf("pmessage[2]: expected %q, got %q", channel, arr[2].Str)
	}
	if string(arr[3].Str) != payload {
		t.Fatalf("pmessage[3]: expected %q, got %q", payload, arr[3].Str)
	}
}

// ---------------------------------------------------------------------------
// Section 8: RESP Edge Cases (C-findings)
// ---------------------------------------------------------------------------

func TestRESP2_IntegerOverflow(t *testing.T) {
	c := dialRedis(t)
	// Send a raw integer reply that overflows int64. We cannot make Redis
	// produce this — it caps at int64. Instead we validate that the parser
	// handles large integers that fit int64.
	k := uniqueKey(t, "intovf")
	cleanupKey(t, c, k)

	c.Send("SET", k, "9223372036854775806") // int64 max - 1
	c.ExpectOK(t)

	c.Send("INCR", k)
	v := c.ReadValue(t)
	if v.Type != protocol.TyInt {
		t.Fatalf("expected integer, got %v", v.Type)
	}
	if v.Int != 9223372036854775807 { // int64 max
		t.Fatalf("expected int64 max, got %d", v.Int)
	}

	// One more INCR should overflow on the server side and return an error.
	c.Send("INCR", k)
	v = c.ReadValue(t)
	if v.Type != protocol.TyError {
		// Redis 7 returns ERR for overflow.
		t.Logf("NOTE: INCR past int64 max returned %v (expected error)", v.Type)
	}
}

func TestRESP3_AttributePrefacedReply(t *testing.T) {
	c := dialRedisRESP3(t)
	// Most servers don't emit attribute frames. Verify the connection is
	// functional in RESP3 mode — attribute frames would be transparently
	// skipped by the parser (tested in unit tests and fuzz).
	k := uniqueKey(t, "attr")
	cleanupKey(t, c, k)

	c.Send("SET", k, "attrval")
	c.ExpectOK(t)

	c.Send("GET", k)
	v := c.ReadValue(t)
	if v.Type == protocol.TyAttr {
		// If an attribute frame somehow appeared, read the real reply.
		v = c.ReadValue(t)
	}
	if v.Type != protocol.TyBulk || string(v.Str) != "attrval" {
		t.Fatalf("GET after potential attribute: got %v %q", v.Type, v.Str)
	}
}

func TestRESP3_PushOnCmdConn(t *testing.T) {
	c := dialRedisRESP3(t)

	// Enable CLIENT TRACKING if supported. This causes the server to
	// send push invalidation messages on the same command connection.
	c.Send("CLIENT", "TRACKING", "ON")
	v := c.ReadValue(t)
	if v.Type == protocol.TyError {
		errStr := string(v.Str)
		if strings.Contains(errStr, "unknown") || strings.Contains(errStr, "ERR") {
			t.Skipf("server does not support CLIENT TRACKING: %s", errStr)
		}
		t.Fatalf("CLIENT TRACKING ON: %s", errStr)
	}

	// Access a key to cache it.
	k := uniqueKey(t, "tracking")
	cleanupKey(t, c, k)
	c.Send("SET", k, "initial")
	c.ExpectOK(t)
	c.Send("GET", k)
	_ = c.ReadValue(t) // cache the key

	// Modify the key from a second connection to trigger invalidation.
	c2 := dialRedis(t)
	c2.Send("SET", k, "modified")
	c2.ExpectOK(t)

	// The server may send a push invalidation frame. Read the next
	// command response — the connection must not crash.
	c.Send("PING")
	v = c.ReadValue(t)
	// We may get a push frame before PONG, or just PONG.
	if v.Type == protocol.TyPush {
		// Push frame received — read the actual PONG.
		v = c.ReadValue(t)
	}
	if v.Type != protocol.TySimple || string(v.Str) != "PONG" {
		t.Fatalf("PING after tracking: expected PONG, got %v %q", v.Type, v.Str)
	}

	// Disable tracking.
	c.Send("CLIENT", "TRACKING", "OFF")
	_ = c.ReadValue(t)
}

func TestCmd_PipelinePartialFailure(t *testing.T) {
	c := dialRedis(t)
	k := uniqueKey(t, "pipefail")
	cleanupKey(t, c, k)

	// Set up a string key so list operations on it fail.
	c.Send("SET", k, "notalist")
	c.ExpectOK(t)

	// Build a 100-command pipeline where command 50 is invalid (LPUSH on
	// a string key).
	w := protocol.NewWriter()
	for i := 0; i < 100; i++ {
		if i == 49 { // 0-indexed, so command 50 in 1-indexed
			w.AppendCommand("LPUSH", k, "item")
		} else {
			w.AppendCommand1("PING")
		}
	}
	c.SendRaw(w.Bytes())

	errCount := 0
	okCount := 0
	for i := 0; i < 100; i++ {
		v := c.ReadValue(t)
		if v.Type == protocol.TyError || v.Type == protocol.TyBlobErr {
			errCount++
			if i != 49 {
				t.Errorf("unexpected error at command %d: %s", i, v.Str)
			}
		} else if v.Type == protocol.TySimple && string(v.Str) == "PONG" {
			okCount++
		} else {
			t.Fatalf("command %d: unexpected reply type %v", i, v.Type)
		}
	}
	if errCount != 1 {
		t.Errorf("expected exactly 1 error, got %d", errCount)
	}
	if okCount != 99 {
		t.Errorf("expected 99 successful replies, got %d", okCount)
	}
}
