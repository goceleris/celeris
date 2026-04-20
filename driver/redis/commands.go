package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/driver/redis/protocol"
)

// do acquires a conn, runs args, and invokes fn with the reply before the
// conn is released. The conn is discarded if ctx is canceled mid-flight
// because the server may still send a stale reply.
func (c *Client) do(ctx context.Context, fn func(v protocol.Value) error, args ...string) error {
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	req, err := conn.exec(ctx, args...)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.pool.discardCmd(conn)
		} else {
			c.pool.releaseCmd(conn)
		}
		return err
	}
	defer conn.releaseResult(req)
	if req.resultErr != nil {
		c.pool.releaseCmd(conn)
		return req.resultErr
	}
	ferr := fn(req.result)
	c.pool.releaseCmd(conn)
	return ferr
}

// doRead is the zero-closure fast path for Get/GetBytes/HGet/HGetAll/etc.
// — any read that returns a single Value. Avoids the per-call heap-allocated
// closure that `do` requires (the closure captures the caller's output var,
// forcing it to escape). Callers must decode req.result themselves and call
// releaseResult before the conn is released.
//
// Returns (result, err). The result aliases the conn's reader buffer and
// is valid only until releaseResult is called — callers that retain bytes
// must copy via asBytes / copyValueDetached first.
func (c *Client) doRead(ctx context.Context, args ...string) (protocol.Value, *redisRequest, *redisConn, error) {
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return protocol.Value{}, nil, nil, err
	}
	req, err := conn.exec(ctx, args...)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.pool.discardCmd(conn)
		} else {
			c.pool.releaseCmd(conn)
		}
		return protocol.Value{}, nil, nil, err
	}
	if req.resultErr != nil {
		rerr := req.resultErr
		conn.releaseResult(req)
		c.pool.releaseCmd(conn)
		return protocol.Value{}, nil, nil, rerr
	}
	return req.result, req, conn, nil
}

// release finalises a doRead call — must be paired with every successful
// doRead return.
func (c *Client) releaseDoRead(req *redisRequest, conn *redisConn) {
	conn.releaseResult(req)
	c.pool.releaseCmd(conn)
}

// ---------- decoding helpers ----------

func asString(v protocol.Value) (string, error) {
	switch v.Type {
	case protocol.TyBulk, protocol.TySimple:
		return string(v.Str), nil
	case protocol.TyVerbatim:
		return string(stripVerbatimPrefix(v.Str)), nil
	case protocol.TyNull:
		return "", ErrNil
	case protocol.TyInt:
		return strconv.FormatInt(v.Int, 10), nil
	default:
		return "", fmt.Errorf("celeris-redis: expected string reply, got %s", v.Type)
	}
}

// stripVerbatimPrefix drops the 4-byte "xxx:" RESP3 verbatim-string prefix
// when present. The prefix's 3-char token ("txt", "mkd", ...) describes the
// payload format and is not part of the returned string value.
func stripVerbatimPrefix(b []byte) []byte {
	if len(b) >= 4 && b[3] == ':' {
		return b[4:]
	}
	return b
}

func asBytes(v protocol.Value) ([]byte, error) {
	switch v.Type {
	case protocol.TyBulk, protocol.TySimple:
		out := make([]byte, len(v.Str))
		copy(out, v.Str)
		return out, nil
	case protocol.TyVerbatim:
		s := stripVerbatimPrefix(v.Str)
		out := make([]byte, len(s))
		copy(out, s)
		return out, nil
	case protocol.TyNull:
		return nil, ErrNil
	default:
		return nil, fmt.Errorf("celeris-redis: expected string reply, got %s", v.Type)
	}
}

func asInt(v protocol.Value) (int64, error) {
	switch v.Type {
	case protocol.TyInt:
		return v.Int, nil
	case protocol.TyBulk, protocol.TySimple:
		return strconv.ParseInt(string(v.Str), 10, 64)
	case protocol.TyNull:
		return 0, ErrNil
	default:
		return 0, fmt.Errorf("celeris-redis: expected int reply, got %s", v.Type)
	}
}

func asFloat(v protocol.Value) (float64, error) {
	switch v.Type {
	case protocol.TyDouble:
		return v.Float, nil
	case protocol.TyBulk, protocol.TySimple:
		return strconv.ParseFloat(string(v.Str), 64)
	case protocol.TyNull:
		return 0, ErrNil
	default:
		return 0, fmt.Errorf("celeris-redis: expected float reply, got %s", v.Type)
	}
}

func asBool(v protocol.Value) (bool, error) {
	switch v.Type {
	case protocol.TyBool:
		return v.Bool, nil
	case protocol.TyInt:
		return v.Int != 0, nil
	case protocol.TyBulk, protocol.TySimple:
		switch string(v.Str) {
		case "1", "true", "TRUE", "True":
			return true, nil
		case "0", "false", "FALSE", "False":
			return false, nil
		}
		return false, fmt.Errorf("celeris-redis: cannot decode bulk %q as bool", v.Str)
	case protocol.TyNull:
		return false, ErrNil
	}
	return false, fmt.Errorf("celeris-redis: expected bool reply, got %s", v.Type)
}

func asStringSlice(v protocol.Value) ([]string, error) {
	switch v.Type {
	case protocol.TyArray, protocol.TySet:
		out := make([]string, len(v.Array))
		for i, elem := range v.Array {
			switch elem.Type {
			case protocol.TyNull:
				out[i] = ""
			default:
				out[i] = string(elem.Str)
			}
		}
		return out, nil
	case protocol.TyNull:
		// Return ErrNil explicitly rather than (nil, nil) — a
		// caller that checks only `err == nil` should not confuse
		// a genuine miss with an empty-but-present array. Matches
		// asInt / asFloat / asString behaviour.
		return nil, ErrNil
	}
	return nil, fmt.Errorf("celeris-redis: expected array reply, got %s", v.Type)
}

func asStringMap(v protocol.Value) (map[string]string, error) {
	if v.Type == protocol.TyNull {
		return nil, ErrNil
	}
	switch v.Type {
	case protocol.TyMap:
		out := make(map[string]string, len(v.Map))
		for _, kv := range v.Map {
			out[string(kv.K.Str)] = string(kv.V.Str)
		}
		return out, nil
	case protocol.TyArray:
		if len(v.Array)%2 != 0 {
			return nil, errors.New("celeris-redis: map array has odd length")
		}
		out := make(map[string]string, len(v.Array)/2)
		for i := 0; i < len(v.Array); i += 2 {
			out[string(v.Array[i].Str)] = string(v.Array[i+1].Str)
		}
		return out, nil
	case protocol.TyNull:
		return nil, ErrNil
	}
	return nil, fmt.Errorf("celeris-redis: expected map reply, got %s", v.Type)
}

// unsafeStringFromBytes reinterprets a []byte as a string without
// copying. The caller MUST guarantee that the returned string is only
// used for read-only operations that complete before the backing slice
// is mutated or freed. Used on the SetBytes / HSetBytes fast paths
// where the written bytes are consumed synchronously by the RESP
// writer + TCP Write and never retained.
func unsafeStringFromBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

// argify converts a value to a string arg. Used by variadic commands.
func argify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// ==================== Strings ====================

// Get retrieves the string value at key. Returns [ErrNil] when the key is
// missing.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	v, req, conn, err := c.doRead(ctx, "GET", key)
	if err != nil {
		return "", err
	}
	s, derr := asString(v)
	c.releaseDoRead(req, conn)
	return s, derr
}

// GetBytes is the []byte variant of Get. The returned slice is a fresh copy.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	v, req, conn, err := c.doRead(ctx, "GET", key)
	if err != nil {
		return nil, err
	}
	b, derr := asBytes(v)
	c.releaseDoRead(req, conn)
	return b, derr
}

// Set stores value at key. If expiration > 0, an EX argument is appended with
// whole-second granularity (or PX for sub-second).
func (c *Client) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	args := []string{"SET", key, argify(value)}
	args = appendExpire(args, expiration)
	return c.do(ctx, func(protocol.Value) error { return nil }, args...)
}

// SetBytes is the allocation-lean variant of [Client.Set]. It converts the
// caller-owned []byte into a string without copying via unsafe.String —
// safe because the RESP writer only reads the bytes to stream to the wire
// and does not retain the string past the round trip. Skips argify's
// interface type switch + allocating string(x) for the []byte case.
func (c *Client) SetBytes(ctx context.Context, key string, value []byte, expiration time.Duration) error {
	args := []string{"SET", key, unsafeStringFromBytes(value)}
	args = appendExpire(args, expiration)
	return c.do(ctx, func(protocol.Value) error { return nil }, args...)
}

// SetNX sets key only if it does not exist.
func (c *Client) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	args := []string{"SET", key, argify(value)}
	args = appendExpire(args, expiration)
	args = append(args, "NX")
	var ok bool
	err := c.do(ctx, func(v protocol.Value) error {
		// SET NX returns +OK or $-1 depending on RESP2/3.
		if v.Type == protocol.TyNull {
			ok = false
			return nil
		}
		ok = v.Type == protocol.TySimple && string(v.Str) == "OK"
		return nil
	}, args...)
	return ok, err
}

func appendExpire(args []string, d time.Duration) []string {
	if d <= 0 {
		return args
	}
	if d >= time.Second && d%time.Second == 0 {
		args = append(args, "EX", strconv.FormatInt(int64(d/time.Second), 10))
	} else {
		args = append(args, "PX", strconv.FormatInt(d.Milliseconds(), 10))
	}
	return args
}

// Del removes one or more keys. Returns the count of deleted keys.
func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	args := append([]string{"DEL"}, keys...)
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// Exists returns the number of keys that exist among the given set.
func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	args := append([]string{"EXISTS"}, keys...)
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// Incr atomically increments an integer key.
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "INCR", key)
	return out, err
}

// Decr atomically decrements an integer key.
func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "DECR", key)
	return out, err
}

// Expire sets a TTL on a key. If expiration is zero or negative, Expire
// calls PERSIST instead (removes the TTL).
func (c *Client) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	if expiration <= 0 {
		return c.Persist(ctx, key)
	}
	secs := int64(expiration / time.Second)
	if secs <= 0 {
		secs = 1 // sub-second positive duration rounds up to 1s
	}
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "EXPIRE", key, strconv.FormatInt(secs, 10))
	return out, err
}

// TTL returns the remaining TTL; -1 if no TTL, -2 if key is missing.
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	var out time.Duration
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		if e != nil {
			return e
		}
		out = time.Duration(n) * time.Second
		return nil
	}, "TTL", key)
	return out, err
}

// MGet fetches multiple keys; missing keys yield empty strings.
func (c *Client) MGet(ctx context.Context, keys ...string) ([]string, error) {
	args := append([]string{"MGET"}, keys...)
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// ==================== Hashes ====================

// HGet retrieves a single field.
func (c *Client) HGet(ctx context.Context, key, field string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "HGET", key, field)
	return out, err
}

// HSet sets one or more field/value pairs. values is a flat list [f1, v1,
// f2, v2, ...].
func (c *Client) HSet(ctx context.Context, key string, values ...any) (int64, error) {
	if len(values) == 0 || len(values)%2 != 0 {
		return 0, errors.New("celeris-redis: HSet requires an even number of values")
	}
	args := make([]string, 0, 2+len(values))
	args = append(args, "HSET", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// HDel removes one or more fields.
func (c *Client) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	args := make([]string, 0, 2+len(fields))
	args = append(args, "HDEL", key)
	args = append(args, fields...)
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// HGetAll returns all fields and values in the hash.
func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	var out map[string]string
	err := c.do(ctx, func(v protocol.Value) error {
		m, e := asStringMap(v)
		out = m
		return e
	}, "HGETALL", key)
	return out, err
}

// HExists checks whether a field exists.
func (c *Client) HExists(ctx context.Context, key, field string) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "HEXISTS", key, field)
	return out, err
}

// HKeys returns the field names.
func (c *Client) HKeys(ctx context.Context, key string) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "HKEYS", key)
	return out, err
}

// HVals returns the field values.
func (c *Client) HVals(ctx context.Context, key string) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "HVALS", key)
	return out, err
}

// ==================== Lists ====================

// LPush prepends one or more values.
func (c *Client) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	args := make([]string, 0, 2+len(values))
	args = append(args, "LPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// RPush appends one or more values.
func (c *Client) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	args := make([]string, 0, 2+len(values))
	args = append(args, "RPUSH", key)
	for _, v := range values {
		args = append(args, argify(v))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// LPop removes and returns the head.
func (c *Client) LPop(ctx context.Context, key string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "LPOP", key)
	return out, err
}

// RPop removes and returns the tail.
func (c *Client) RPop(ctx context.Context, key string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "RPOP", key)
	return out, err
}

// LRange returns a slice of the list.
func (c *Client) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "LRANGE", key, strconv.FormatInt(start, 10), strconv.FormatInt(stop, 10))
	return out, err
}

// LLen returns the list length.
func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "LLEN", key)
	return out, err
}

// ==================== Sets ====================

// SAdd adds members to a set.
func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	args := make([]string, 0, 2+len(members))
	args = append(args, "SADD", key)
	for _, m := range members {
		args = append(args, argify(m))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// SRem removes members.
func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	args := make([]string, 0, 2+len(members))
	args = append(args, "SREM", key)
	for _, m := range members {
		args = append(args, argify(m))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// SMembers returns all members.
func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "SMEMBERS", key)
	return out, err
}

// SIsMember checks membership.
func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		b, e := asBool(v)
		if e != nil {
			// RESP2 returns 1/0 as Int.
			n, ie := asInt(v)
			if ie != nil {
				return e
			}
			out = n == 1
			return nil
		}
		out = b
		return nil
	}, "SISMEMBER", key, argify(member))
	return out, err
}

// SCard returns the cardinality.
func (c *Client) SCard(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "SCARD", key)
	return out, err
}

// ==================== Sorted sets ====================

// Z is a score/member tuple passed to ZAdd.
type Z struct {
	Score  float64
	Member any
}

// ZAdd adds one or more members.
func (c *Client) ZAdd(ctx context.Context, key string, members ...Z) (int64, error) {
	args := make([]string, 0, 2+2*len(members))
	args = append(args, "ZADD", key)
	for _, m := range members {
		args = append(args, strconv.FormatFloat(m.Score, 'f', -1, 64), argify(m.Member))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// ZRange returns members in index order.
func (c *Client) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "ZRANGE", key, strconv.FormatInt(start, 10), strconv.FormatInt(stop, 10))
	return out, err
}

// ZRangeBy passes the LIMIT form of ZRANGEBYSCORE.
type ZRangeBy struct {
	Min, Max      string
	Offset, Count int64
}

// ZRangeByScore returns members with scores in [min,max] with optional
// LIMIT.
func (c *Client) ZRangeByScore(ctx context.Context, key string, opt *ZRangeBy) ([]string, error) {
	if opt == nil {
		return nil, errors.New("celeris-redis: ZRangeByScore opt is nil")
	}
	args := []string{"ZRANGEBYSCORE", key, opt.Min, opt.Max}
	if opt.Count != 0 {
		args = append(args, "LIMIT", strconv.FormatInt(opt.Offset, 10), strconv.FormatInt(opt.Count, 10))
	}
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// ZRem removes members.
func (c *Client) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	args := make([]string, 0, 2+len(members))
	args = append(args, "ZREM", key)
	for _, m := range members {
		args = append(args, argify(m))
	}
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, args...)
	return out, err
}

// ZScore returns the score of member.
func (c *Client) ZScore(ctx context.Context, key, member string) (float64, error) {
	var out float64
	err := c.do(ctx, func(v protocol.Value) error {
		f, e := asFloat(v)
		out = f
		return e
	}, "ZSCORE", key, member)
	return out, err
}

// ZCard returns the cardinality.
func (c *Client) ZCard(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "ZCARD", key)
	return out, err
}

// ==================== Keys ====================

// Type returns the type of key ("string", "list", "hash", ...).
func (c *Client) Type(ctx context.Context, key string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = strings.TrimSpace(s)
		return e
	}, "TYPE", key)
	return out, err
}

// Rename renames a key.
func (c *Client) Rename(ctx context.Context, key, newKey string) error {
	return c.do(ctx, func(protocol.Value) error { return nil }, "RENAME", key, newKey)
}

// RandomKey returns a random key.
func (c *Client) RandomKey(ctx context.Context) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "RANDOMKEY")
	return out, err
}

// PExpire sets a TTL in milliseconds. If ms is zero or negative, PExpire
// calls PERSIST instead (removes the TTL).
func (c *Client) PExpire(ctx context.Context, key string, ms time.Duration) (bool, error) {
	if ms <= 0 {
		return c.Persist(ctx, key)
	}
	millis := ms.Milliseconds()
	if millis <= 0 {
		millis = 1 // sub-millisecond positive duration rounds up to 1ms
	}
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "PEXPIRE", key, strconv.FormatInt(millis, 10))
	return out, err
}

// ExpireAt sets an absolute expiration timestamp (second granularity).
func (c *Client) ExpireAt(ctx context.Context, key string, at time.Time) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "EXPIREAT", key, strconv.FormatInt(at.Unix(), 10))
	return out, err
}

// PExpireAt sets an absolute expiration timestamp (millisecond granularity).
func (c *Client) PExpireAt(ctx context.Context, key string, at time.Time) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "PEXPIREAT", key, strconv.FormatInt(at.UnixMilli(), 10))
	return out, err
}

// Persist removes the TTL from a key. Returns true if a TTL was removed.
func (c *Client) Persist(ctx context.Context, key string) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "PERSIST", key)
	return out, err
}

// ==================== Pub/Sub ====================

// Publish sends message to channel via PUBLISH. Returns the number of
// subscribers that received the message.
func (c *Client) Publish(ctx context.Context, channel, message string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "PUBLISH", channel, message)
	return out, err
}

// ==================== Scripting ====================

// Eval runs a Lua script server-side. keys and args may be empty; numkeys is
// inferred from len(keys). The returned *protocol.Value is detached and safe
// to retain.
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (*protocol.Value, error) {
	return c.evalCmd(ctx, "EVAL", script, keys, args...)
}

// EvalSHA runs a previously-loaded script by sha1. Returns NOSCRIPT as
// *RedisError when the server does not have the script cached.
func (c *Client) EvalSHA(ctx context.Context, sha string, keys []string, args ...any) (*protocol.Value, error) {
	return c.evalCmd(ctx, "EVALSHA", sha, keys, args...)
}

func (c *Client) evalCmd(ctx context.Context, verb, scriptOrSHA string, keys []string, args ...any) (*protocol.Value, error) {
	total := 3 + len(keys) + len(args)
	cmd := make([]string, 0, total)
	cmd = append(cmd, verb, scriptOrSHA, strconv.Itoa(len(keys)))
	cmd = append(cmd, keys...)
	for _, a := range args {
		cmd = append(cmd, argify(a))
	}
	var out protocol.Value
	err := c.do(ctx, func(v protocol.Value) error {
		// Deep-copy the result so aggregate slices (Array, Map) do not
		// alias the reader buffer. dispatch already calls
		// copyValueDetached for non-pipeline requests, but we defensively
		// copy here because the caller retains the Value past
		// releaseResult.
		out = copyValueDetached(v)
		return nil
	}, cmd...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ScriptLoad caches a script on the server and returns its SHA1 digest.
func (c *Client) ScriptLoad(ctx context.Context, script string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "SCRIPT", "LOAD", script)
	return out, err
}

// ==================== Watch ====================

// Watch acquires a dedicated connection, issues WATCH on keys, and invokes fn
// with a Tx pinned to that connection. The connection is released when fn
// returns (after Exec or Discard). This is the only correct way to use WATCH
// on a pooled client because the WATCH state is per-connection.
//
// Usage:
//
//	err := client.Watch(ctx, func(tx *Tx) error {
//	    // Read current value (on the same pinned conn via the Tx's client).
//	    // Queue commands:
//	    tx.Set("k", "new", 0)
//	    return tx.Exec(ctx)
//	}, "k")
//
// If the WATCHed keys change before EXEC, Exec returns [ErrTxAborted].
func (c *Client) Watch(ctx context.Context, fn func(tx *Tx) error, keys ...string) error {
	if len(keys) == 0 {
		return errors.New("celeris-redis: Watch requires at least one key")
	}
	conn, err := c.pool.acquireCmd(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	conn.dirty.Store(true)

	// Issue WATCH on the pinned conn.
	args := append([]string{"WATCH"}, keys...)
	req, err := conn.exec(ctx, args...)
	if err != nil {
		c.pool.discardCmd(conn)
		return err
	}
	conn.releaseResult(req)
	if req.resultErr != nil {
		c.pool.discardCmd(conn)
		return req.resultErr
	}

	tx := &Tx{
		client: c,
		conn:   conn,
		w:      protocol.NewWriter(),
	}
	err = fn(tx)
	// If fn did not call Exec or Discard, clean up the conn.
	if !tx.done {
		tx.done = true
		conn.dirty.Store(false)
		c.pool.releaseCmd(conn)
	}
	return err
}

// ==================== Additional String commands ====================

// MSet sets multiple key-value pairs atomically. pairs is a flat list
// [k1, v1, k2, v2, ...].
func (c *Client) MSet(ctx context.Context, pairs ...any) error {
	if len(pairs) == 0 || len(pairs)%2 != 0 {
		return errors.New("celeris-redis: MSet requires an even number of arguments")
	}
	args := make([]string, 0, 1+len(pairs))
	args = append(args, "MSET")
	for _, p := range pairs {
		args = append(args, argify(p))
	}
	return c.do(ctx, func(protocol.Value) error { return nil }, args...)
}

// IncrBy increments key by val and returns the new value.
func (c *Client) IncrBy(ctx context.Context, key string, val int64) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "INCRBY", key, strconv.FormatInt(val, 10))
	return out, err
}

// IncrByFloat increments key by val and returns the new value.
func (c *Client) IncrByFloat(ctx context.Context, key string, val float64) (float64, error) {
	var out float64
	err := c.do(ctx, func(v protocol.Value) error {
		f, e := asFloat(v)
		out = f
		return e
	}, "INCRBYFLOAT", key, strconv.FormatFloat(val, 'f', -1, 64))
	return out, err
}

// DecrBy decrements key by val and returns the new value.
func (c *Client) DecrBy(ctx context.Context, key string, val int64) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "DECRBY", key, strconv.FormatInt(val, 10))
	return out, err
}

// GetDel gets the value of key and deletes it. Returns [ErrNil] if missing.
func (c *Client) GetDel(ctx context.Context, key string) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "GETDEL", key)
	return out, err
}

// SetEX sets key to value with the given TTL (second granularity). This is
// the atomic equivalent of SET + EXPIRE.
func (c *Client) SetEX(ctx context.Context, key string, value any, ttl time.Duration) error {
	secs := int64(ttl / time.Second)
	if secs <= 0 {
		secs = 1
	}
	return c.do(ctx, func(protocol.Value) error { return nil },
		"SETEX", key, strconv.FormatInt(secs, 10), argify(value))
}

// Append appends value to key and returns the new string length.
func (c *Client) Append(ctx context.Context, key string, value string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "APPEND", key, value)
	return out, err
}

// ==================== Additional List commands ====================

// LIndex returns the element at index in the list stored at key.
func (c *Client) LIndex(ctx context.Context, key string, index int64) (string, error) {
	var out string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asString(v)
		out = s
		return e
	}, "LINDEX", key, strconv.FormatInt(index, 10))
	return out, err
}

// LRem removes count occurrences of element from the list at key.
func (c *Client) LRem(ctx context.Context, key string, count int64, element any) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "LREM", key, strconv.FormatInt(count, 10), argify(element))
	return out, err
}

// ==================== Additional Set commands ====================

// SInter returns the intersection of all given sets.
func (c *Client) SInter(ctx context.Context, keys ...string) ([]string, error) {
	args := append([]string{"SINTER"}, keys...)
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// SUnion returns the union of all given sets.
func (c *Client) SUnion(ctx context.Context, keys ...string) ([]string, error) {
	args := append([]string{"SUNION"}, keys...)
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// SDiff returns the difference of the first set with all successive sets.
func (c *Client) SDiff(ctx context.Context, keys ...string) ([]string, error) {
	args := append([]string{"SDIFF"}, keys...)
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// ==================== Additional Sorted Set commands ====================

// ZRank returns the rank (0-based) of member in the sorted set at key.
func (c *Client) ZRank(ctx context.Context, key, member string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "ZRANK", key, member)
	return out, err
}

// ZRevRange returns members in reverse index order.
func (c *Client) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, "ZREVRANGE", key, strconv.FormatInt(start, 10), strconv.FormatInt(stop, 10))
	return out, err
}

// ZCount returns the count of members with scores between minScore and maxScore
// (inclusive string boundaries, e.g. "-inf", "+inf", "1", "(5").
func (c *Client) ZCount(ctx context.Context, key, minScore, maxScore string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "ZCOUNT", key, minScore, maxScore)
	return out, err
}

// ZIncrBy increments the score of member in the sorted set at key by incr.
func (c *Client) ZIncrBy(ctx context.Context, key string, incr float64, member string) (float64, error) {
	var out float64
	err := c.do(ctx, func(v protocol.Value) error {
		f, e := asFloat(v)
		out = f
		return e
	}, "ZINCRBY", key, strconv.FormatFloat(incr, 'f', -1, 64), member)
	return out, err
}

// ==================== Additional Hash commands ====================

// HIncrBy increments a hash field by an integer delta.
func (c *Client) HIncrBy(ctx context.Context, key, field string, incr int64) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "HINCRBY", key, field, strconv.FormatInt(incr, 10))
	return out, err
}

// HIncrByFloat increments a hash field by a float delta.
func (c *Client) HIncrByFloat(ctx context.Context, key, field string, incr float64) (float64, error) {
	var out float64
	err := c.do(ctx, func(v protocol.Value) error {
		f, e := asFloat(v)
		out = f
		return e
	}, "HINCRBYFLOAT", key, field, strconv.FormatFloat(incr, 'f', -1, 64))
	return out, err
}

// HLen returns the number of fields in the hash at key.
func (c *Client) HLen(ctx context.Context, key string) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "HLEN", key)
	return out, err
}

// HSetNX sets a hash field only if it does not exist.
func (c *Client) HSetNX(ctx context.Context, key, field string, value any) (bool, error) {
	var out bool
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n == 1
		return e
	}, "HSETNX", key, field, argify(value))
	return out, err
}

// HMGet returns the values of the specified fields in the hash at key.
// Missing fields yield empty strings.
func (c *Client) HMGet(ctx context.Context, key string, fields ...string) ([]string, error) {
	args := make([]string, 0, 2+len(fields))
	args = append(args, "HMGET", key)
	args = append(args, fields...)
	var out []string
	err := c.do(ctx, func(v protocol.Value) error {
		s, e := asStringSlice(v)
		out = s
		return e
	}, args...)
	return out, err
}

// ==================== Additional Key commands ====================

// PTTL returns the remaining TTL in milliseconds; -1 if no TTL, -2 if key
// is missing.
func (c *Client) PTTL(ctx context.Context, key string) (time.Duration, error) {
	var out time.Duration
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		if e != nil {
			return e
		}
		out = time.Duration(n) * time.Millisecond
		return nil
	}, "PTTL", key)
	return out, err
}

// ==================== Server commands ====================

// FlushDB removes all keys from the current database.
func (c *Client) FlushDB(ctx context.Context) error {
	return c.do(ctx, func(protocol.Value) error { return nil }, "FLUSHDB")
}

// DBSize returns the number of keys in the current database.
func (c *Client) DBSize(ctx context.Context) (int64, error) {
	var out int64
	err := c.do(ctx, func(v protocol.Value) error {
		n, e := asInt(v)
		out = n
		return e
	}, "DBSIZE")
	return out, err
}
