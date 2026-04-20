package memcached

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// exptime converts a Go duration to a memcached expiration second-count.
//
// Memcached accepts two forms:
//   - 0              → no expiration.
//   - 1..2592000     → relative seconds (up to 30 days).
//   - > 2592000      → absolute unix timestamp.
//
// The driver takes care of this transform: callers pass a relative
// time.Duration and the driver switches to an absolute timestamp when the
// duration exceeds 30 days.
func exptime(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	secs := int64(ttl / time.Second)
	if secs <= 0 {
		secs = 1 // sub-second positive duration rounds up to 1s
	}
	const relCap = 60 * 60 * 24 * 30
	if secs > relCap {
		return time.Now().Unix() + secs
	}
	return secs
}

// exptimeFromSeconds is the uint32 variant used by the binary protocol
// packer, which allocates the exptime field as big-endian uint32. Same rules
// as [exptime] but saturating to math.MaxUint32 if the absolute timestamp
// would overflow 32 bits (a concern around the year 2106).
func exptimeFromSeconds(s int64) uint32 {
	if s < 0 {
		return 0
	}
	if s > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(s)
}

// validateKey enforces the text-protocol restrictions: 1..250 bytes, no
// whitespace, no control bytes. Binary is stricter only on length (250
// bytes). Applied uniformly across both dialects so callers can switch
// without changing their key-hygiene assumptions.
func validateKey(key string) error {
	if len(key) == 0 || len(key) > 250 {
		return ErrMalformedKey
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if b <= ' ' || b == 0x7f {
			return ErrMalformedKey
		}
	}
	return nil
}

// asBytes converts a user-supplied value to its wire-level byte form. Used
// by Set/Add/Replace/CAS/etc.
func asBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	case int:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	case int32:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case uint:
		return []byte(strconv.FormatUint(uint64(x), 10)), nil
	case uint64:
		return []byte(strconv.FormatUint(x, 10)), nil
	case uint32:
		return []byte(strconv.FormatUint(uint64(x), 10)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(x), 'f', -1, 32)), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'f', -1, 64)), nil
	case bool:
		if x {
			return []byte{'1'}, nil
		}
		return []byte{'0'}, nil
	case nil:
		return nil, nil
	case fmt.Stringer:
		return []byte(x.String()), nil
	default:
		return nil, fmt.Errorf("celeris-memcached: unsupported value type %T", v)
	}
}

// run acquires a conn, runs fn with it, and releases the conn. On ctx
// cancellation or context deadline the conn is discarded because the server
// may still send a stale reply.
func (c *Client) run(ctx context.Context, fn func(ctx context.Context, conn *memcachedConn) error) error {
	conn, err := c.pool.acquire(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	ferr := fn(ctx, conn)
	if ferr != nil && (errors.Is(ferr, context.Canceled) || errors.Is(ferr, context.DeadlineExceeded)) {
		c.pool.discard(conn)
		return ferr
	}
	c.pool.release(conn)
	return ferr
}

// ---------- storage (set/add/replace/append/prepend) ----------

// storeCmd routes a storage command through the active protocol.
func (c *Client) storeCmd(ctx context.Context, cmd string, key string, value []byte, ttl time.Duration) error {
	if err := validateKey(key); err != nil {
		return err
	}
	return c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			opcode, err := binaryStorageOp(cmd)
			if err != nil {
				return err
			}
			exp := exptimeFromSeconds(exptime(ttl))
			req, err := conn.execBinary(ctx, opcode, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				// For append/prepend, opcode has no exptime/flags extras.
				if opcode == protocol.OpAppend || opcode == protocol.OpPrepend {
					return w.AppendConcat(opcode, key, value, 0, opaque)
				}
				return w.AppendStorage(opcode, key, value, 0, exp, 0, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			return translateBinaryStorage(req)
		}
		req, err := conn.execText(ctx, kindStatusOnly, func(w *protocol.TextWriter) []byte {
			return w.AppendStorage(cmd, key, 0, exptime(ttl), value, 0, false)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		return translateTextStorage(req)
	})
}

// Set stores value at key, overwriting any existing value.
func (c *Client) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	b, err := asBytes(value)
	if err != nil {
		return err
	}
	return c.storeCmd(ctx, "set", key, b, ttl)
}

// SetBytes is the allocation-lean variant of [Client.Set] for callers that
// already have the value as bytes. Skips the `any` interface boxing and
// the asBytes type switch — see the ratelimit/memcachedstore bench gap.
func (c *Client) SetBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.storeCmd(ctx, "set", key, value, ttl)
}

// Add stores value at key only if the key does not already exist. Returns
// [ErrNotStored] when the key is present.
func (c *Client) Add(ctx context.Context, key string, value any, ttl time.Duration) error {
	b, err := asBytes(value)
	if err != nil {
		return err
	}
	return c.storeCmd(ctx, "add", key, b, ttl)
}

// AddBytes is the allocation-lean variant of [Client.Add]. See [Client.SetBytes].
func (c *Client) AddBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.storeCmd(ctx, "add", key, value, ttl)
}

// Replace stores value at key only if the key already exists. Returns
// [ErrNotStored] when the key is missing.
func (c *Client) Replace(ctx context.Context, key string, value any, ttl time.Duration) error {
	b, err := asBytes(value)
	if err != nil {
		return err
	}
	return c.storeCmd(ctx, "replace", key, b, ttl)
}

// ReplaceBytes is the allocation-lean variant of [Client.Replace].
func (c *Client) ReplaceBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.storeCmd(ctx, "replace", key, value, ttl)
}

// Append appends value to the existing value at key. Returns [ErrNotStored]
// when the key is missing.
func (c *Client) Append(ctx context.Context, key, value string) error {
	return c.storeCmd(ctx, "append", key, []byte(value), 0)
}

// Prepend prepends value to the existing value at key. Returns [ErrNotStored]
// when the key is missing.
func (c *Client) Prepend(ctx context.Context, key, value string) error {
	return c.storeCmd(ctx, "prepend", key, []byte(value), 0)
}

// CAS performs a compare-and-swap update: the store succeeds only if the
// server's current CAS token matches casID. Returns (false, [ErrCASConflict])
// on mismatch, (false, [ErrCacheMiss]) if the key is missing, and (true, nil)
// on success.
func (c *Client) CAS(ctx context.Context, key string, value any, casID uint64, ttl time.Duration) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	if casID == 0 {
		return false, ErrInvalidCAS
	}
	b, err := asBytes(value)
	if err != nil {
		return false, err
	}
	return c.casBytes(ctx, key, b, casID, ttl)
}

// CASBytes is the allocation-lean variant of [Client.CAS]. Skips the `any`
// boxing + asBytes type switch for callers that already have []byte.
func (c *Client) CASBytes(ctx context.Context, key string, value []byte, casID uint64, ttl time.Duration) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	if casID == 0 {
		return false, ErrInvalidCAS
	}
	return c.casBytes(ctx, key, value, casID, ttl)
}

// casBytes is the shared CAS implementation for CAS / CASBytes. Both enter
// here after their own validation so the protocol flow is identical.
//
// Rejecting casID=0 is handled by the callers: both the text `cas` command
// and binary OpSet treat a CAS of 0 as "don't check", degrading into an
// unconditional Set — a silent behaviour change that hides caller bugs.
// A legitimate CAS token from Gets is never 0.
func (c *Client) casBytes(ctx context.Context, key string, value []byte, casID uint64, ttl time.Duration) (bool, error) {
	var ok bool
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			exp := exptimeFromSeconds(exptime(ttl))
			req, err := conn.execBinary(ctx, protocol.OpSet, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendStorage(protocol.OpSet, key, value, 0, exp, casID, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				ok = true
				return nil
			case protocol.StatusKeyExists:
				return ErrCASConflict
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindStatusOnly, func(w *protocol.TextWriter) []byte {
			return w.AppendStorage("cas", key, 0, exptime(ttl), value, casID, false)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		switch req.status {
		case protocol.KindStored:
			ok = true
			return nil
		case protocol.KindExists:
			return ErrCASConflict
		case protocol.KindNotFound:
			return ErrCacheMiss
		case protocol.KindNotStored:
			return ErrNotStored
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
	})
	return ok, err
}

// ---------- retrieval ----------

// Get fetches the string value at key. Returns [ErrCacheMiss] when the key
// is missing.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	b, err := c.GetBytes(ctx, key)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GetBytes is the []byte variant of [Client.Get]. The returned slice is a
// fresh copy that the caller owns.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	var out []byte
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinary(ctx, protocol.OpGet, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendGet(protocol.OpGet, key, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				if len(req.values) > 0 {
					out = req.values[0].Data
				}
				return nil
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindGet, func(w *protocol.TextWriter) []byte {
			return w.AppendRetrieval("get", key)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		if len(req.values) == 0 {
			return ErrCacheMiss
		}
		out = req.values[0].Data
		return nil
	})
	return out, err
}

// GetMulti fetches multiple keys in one round trip. Missing keys are omitted
// from the returned map. Each value is a fresh copy owned by the caller.
func (c *Client) GetMulti(ctx context.Context, keys ...string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return nil, err
		}
	}
	out := make(map[string]string, len(keys))
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinaryMulti(ctx,
				func(w *protocol.BinaryWriter) []byte { return encodeGetMultiBatch(conn, w, keys) },
				isGetMultiTerminator,
			)
			defer putRequest(req)
			if err != nil {
				return err
			}
			if req.resultErr != nil {
				return req.resultErr
			}
			for _, v := range req.values {
				out[v.Key] = bytesToString(v.Data)
			}
			return nil
		}
		req, err := conn.execText(ctx, kindGet, func(w *protocol.TextWriter) []byte {
			return w.AppendRetrieval("get", keys...)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		for _, v := range req.values {
			out[v.Key] = string(v.Data)
		}
		return nil
	})
	return out, err
}

// GetMultiBytes is the []byte variant of [Client.GetMulti].
func (c *Client) GetMultiBytes(ctx context.Context, keys ...string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return nil, err
		}
	}
	out := make(map[string][]byte, len(keys))
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinaryMulti(ctx,
				func(w *protocol.BinaryWriter) []byte { return encodeGetMultiBatch(conn, w, keys) },
				isGetMultiTerminator,
			)
			defer putRequest(req)
			if err != nil {
				return err
			}
			if req.resultErr != nil {
				return req.resultErr
			}
			for _, v := range req.values {
				out[v.Key] = v.Data
			}
			return nil
		}
		req, err := conn.execText(ctx, kindGet, func(w *protocol.TextWriter) []byte {
			return w.AppendRetrieval("get", keys...)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		for _, v := range req.values {
			out[v.Key] = v.Data
		}
		return nil
	})
	return out, err
}

// encodeGetMultiBatch emits N OpGetKQ packets followed by one OpNoop
// terminator, all into a single buffer. OpGetKQ is the "quiet get with key"
// variant: hits stream back as separate response packets carrying the key,
// while misses suppress any reply entirely. The trailing OpNoop forces a
// reply even if every key missed, giving dispatch a definite end-of-stream
// marker. Opaque values are not used for matching (we rely on the key
// carried in the response), but we assign monotonically increasing values
// for tracing and to keep the wire identical to what mcrouter / twemproxy
// emit.
func encodeGetMultiBatch(conn *memcachedConn, w *protocol.BinaryWriter, keys []string) []byte {
	for _, k := range keys {
		op := conn.opaque.Add(1)
		_ = w.AppendGet(protocol.OpGetKQ, k, op)
	}
	return w.AppendSimple(protocol.OpNoop, conn.opaque.Add(1))
}

// isGetMultiTerminator returns true for the Noop echo that closes a
// pipelined GetMulti stream.
func isGetMultiTerminator(p protocol.BinaryPacket) bool {
	return p.Header.Opcode == protocol.OpNoop
}

// isStatsTerminator returns true for the zero-body STAT packet that ends a
// stats stream. Per the binary spec, the terminator carries no key and no
// value.
func isStatsTerminator(p protocol.BinaryPacket) bool {
	return p.Header.Opcode == protocol.OpStat &&
		p.Header.KeyLen == 0 && p.Header.BodyLen == 0
}

// CASItem is the key + value + CAS token triple returned by [Client.Gets].
type CASItem struct {
	Key   string
	Value []byte
	Flags uint32
	CAS   uint64
}

// Gets fetches a value along with its CAS token. The returned item's CAS
// field can be passed to [Client.CAS] to perform an atomic update.
func (c *Client) Gets(ctx context.Context, key string) (CASItem, error) {
	if err := validateKey(key); err != nil {
		return CASItem{}, err
	}
	var out CASItem
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			// Binary GET returns the CAS in the response header for every hit.
			req, err := conn.execBinary(ctx, protocol.OpGet, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendGet(protocol.OpGet, key, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				if len(req.values) == 0 {
					return ErrCacheMiss
				}
				v := req.values[0]
				out = CASItem{Key: key, Value: v.Data, Flags: v.Flags, CAS: v.CAS}
				return nil
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindGet, func(w *protocol.TextWriter) []byte {
			return w.AppendRetrieval("gets", key)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		if len(req.values) == 0 {
			return ErrCacheMiss
		}
		v := req.values[0]
		out = CASItem{Key: v.Key, Value: v.Data, Flags: v.Flags, CAS: v.CAS}
		return nil
	})
	return out, err
}

// ---------- arithmetic ----------

// Incr atomically increments the integer value at key by delta and returns
// the new value. The server returns [ErrCacheMiss] if the key is missing
// (use Set first to initialize) or a MemcachedError if the existing value
// is not a decimal integer.
func (c *Client) Incr(ctx context.Context, key string, delta uint64) (uint64, error) {
	return c.arith(ctx, "incr", protocol.OpIncrement, key, delta)
}

// Decr atomically decrements the integer value at key by delta and returns
// the new value. The memcached server clamps the result at 0 (it does not
// wrap to MAX_UINT).
func (c *Client) Decr(ctx context.Context, key string, delta uint64) (uint64, error) {
	return c.arith(ctx, "decr", protocol.OpDecrement, key, delta)
}

func (c *Client) arith(ctx context.Context, cmd string, opcode byte, key string, delta uint64) (uint64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	var out uint64
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			// Binary incr/decr accepts an initial value and an exptime.
			// Passing exptime=0xFFFFFFFF instructs the server NOT to create
			// the key on miss — the closest match to the text protocol's
			// behavior ("NOT_FOUND\r\n").
			req, err := conn.execBinary(ctx, opcode, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendArith(opcode, key, delta, 0, 0xFFFFFFFF, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				// Binary Incr/Decr replies carry the new counter value as an
				// 8-byte big-endian uint64 in the body. A truncated body or
				// missing value indicates a malformed reply from the server
				// — surface that as a protocol error rather than silently
				// returning 0, which would be indistinguishable from a
				// legitimate counter that was just decremented to zero.
				if len(req.values) == 0 || len(req.values[0].Data) < 8 {
					return fmt.Errorf("%w: binary Incr/Decr reply missing 8-byte counter body",
						ErrProtocol)
				}
				b := req.values[0].Data
				out = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
					uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
				return nil
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindArith, func(w *protocol.TextWriter) []byte {
			return w.AppendArith(cmd, key, delta)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		switch req.status {
		case protocol.KindNumber:
			out = req.number
			return nil
		case protocol.KindNotFound:
			return ErrCacheMiss
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
	})
	return out, err
}

// ---------- delete / touch / flush ----------

// Delete removes key from the cache. Returns [ErrCacheMiss] if the key did
// not exist.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	return c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinary(ctx, protocol.OpDelete, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendDelete(key, 0, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				return nil
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindStatusOnly, func(w *protocol.TextWriter) []byte {
			return w.AppendDelete(key)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		switch req.status {
		case protocol.KindDeleted:
			return nil
		case protocol.KindNotFound:
			return ErrCacheMiss
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
	})
}

// Touch updates the expiration of key without fetching the value. Returns
// [ErrCacheMiss] if the key is missing.
func (c *Client) Touch(ctx context.Context, key string, ttl time.Duration) error {
	if err := validateKey(key); err != nil {
		return err
	}
	return c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			exp := exptimeFromSeconds(exptime(ttl))
			req, err := conn.execBinary(ctx, protocol.OpTouch, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendTouch(key, exp, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			switch req.binStatus {
			case protocol.StatusOK:
				return nil
			case protocol.StatusKeyNotFound:
				return ErrCacheMiss
			}
			return req.resultErr
		}
		req, err := conn.execText(ctx, kindStatusOnly, func(w *protocol.TextWriter) []byte {
			return w.AppendTouch(key, exptime(ttl))
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		switch req.status {
		case protocol.KindTouched:
			return nil
		case protocol.KindNotFound:
			return ErrCacheMiss
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
	})
}

// Flush wipes every entry across every slab class. Immediate.
func (c *Client) Flush(ctx context.Context) error {
	return c.flush(ctx, 0)
}

// FlushAfter schedules a flush to take effect after delay. Implemented as
// a relative exptime argument to flush_all / OpFlush extras.
func (c *Client) FlushAfter(ctx context.Context, delay time.Duration) error {
	secs := int64(delay / time.Second)
	if secs < 0 {
		secs = 0
	}
	return c.flush(ctx, secs)
}

func (c *Client) flush(ctx context.Context, delaySecs int64) error {
	return c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinary(ctx, protocol.OpFlush, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendFlush(uint32(delaySecs), opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			if req.binStatus != protocol.StatusOK {
				return req.resultErr
			}
			return nil
		}
		req, err := conn.execText(ctx, kindStatusOnly, func(w *protocol.TextWriter) []byte {
			if delaySecs > 0 {
				return w.AppendFlushAll(delaySecs)
			}
			return w.AppendFlushAll(-1)
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.status == protocol.KindOK {
			return nil
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
	})
}

// ---------- server introspection ----------

// Stats returns the server's general statistics map.
func (c *Client) Stats(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string)
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinaryMulti(ctx,
				func(w *protocol.BinaryWriter) []byte {
					return w.AppendStats("", conn.opaque.Add(1))
				},
				isStatsTerminator,
			)
			defer putRequest(req)
			if err != nil {
				return err
			}
			if req.resultErr != nil {
				return req.resultErr
			}
			for _, s := range req.stats {
				out[s.Name] = s.Value
			}
			return nil
		}
		req, err := conn.execText(ctx, kindStats, func(w *protocol.TextWriter) []byte {
			return w.AppendStats("")
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		for _, s := range req.stats {
			out[s.Name] = s.Value
		}
		return nil
	})
	return out, err
}

// Version returns the server version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out string
	err := c.run(ctx, func(ctx context.Context, conn *memcachedConn) error {
		if conn.binary {
			req, err := conn.execBinary(ctx, protocol.OpVersion, func(w *protocol.BinaryWriter, opaque uint32) []byte {
				return w.AppendSimple(protocol.OpVersion, opaque)
			})
			defer putRequest(req)
			if err != nil {
				return err
			}
			if req.binStatus != protocol.StatusOK {
				return req.resultErr
			}
			if len(req.values) > 0 {
				out = string(req.values[0].Data)
			}
			return nil
		}
		req, err := conn.execText(ctx, kindVersion, func(w *protocol.TextWriter) []byte {
			return w.AppendSimple("version")
		})
		defer putRequest(req)
		if err != nil {
			return err
		}
		if req.resultErr != nil {
			return req.resultErr
		}
		out = req.version
		return nil
	})
	return out, err
}

// Ping issues a VERSION (cheaper than a full stats call) and discards the
// reply. Useful as a liveness probe.
func (c *Client) Ping(ctx context.Context) error {
	conn, err := c.pool.acquire(ctx, workerFromCtx(ctx))
	if err != nil {
		return err
	}
	perr := conn.Ping(ctx)
	if perr != nil {
		if errors.Is(perr, context.Canceled) || errors.Is(perr, context.DeadlineExceeded) {
			c.pool.discard(conn)
			return perr
		}
		c.pool.release(conn)
		return perr
	}
	c.pool.release(conn)
	return nil
}

// ---------- binary helpers ----------

// binaryStorageOp maps a text-protocol command name to the matching binary
// opcode.
func binaryStorageOp(cmd string) (byte, error) {
	switch cmd {
	case "set":
		return protocol.OpSet, nil
	case "add":
		return protocol.OpAdd, nil
	case "replace":
		return protocol.OpReplace, nil
	case "append":
		return protocol.OpAppend, nil
	case "prepend":
		return protocol.OpPrepend, nil
	}
	return 0, fmt.Errorf("celeris-memcached: no binary opcode for %q", cmd)
}

// translateTextStorage converts a text-protocol reply into the appropriate
// Go-level error for storage commands (set / add / replace / append /
// prepend).
func translateTextStorage(req *mcRequest) error {
	if req.resultErr != nil {
		return req.resultErr
	}
	switch req.status {
	case protocol.KindStored:
		return nil
	case protocol.KindNotStored:
		return ErrNotStored
	case protocol.KindExists:
		return ErrCASConflict
	case protocol.KindNotFound:
		return ErrCacheMiss
	}
	return &MemcachedError{Kind: "UNEXPECTED", Msg: req.status.String()}
}

// translateBinaryStorage converts a binary-protocol status code into the
// appropriate Go-level error for storage commands.
func translateBinaryStorage(req *mcRequest) error {
	switch req.binStatus {
	case protocol.StatusOK:
		return nil
	case protocol.StatusKeyNotFound:
		return ErrCacheMiss
	case protocol.StatusKeyExists:
		return ErrCASConflict
	case protocol.StatusItemNotStored:
		return ErrNotStored
	}
	if req.resultErr != nil {
		return req.resultErr
	}
	return &MemcachedError{Status: req.binStatus}
}
