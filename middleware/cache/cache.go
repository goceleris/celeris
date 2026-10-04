// Package-level documentation lives in doc.go.

package cache

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/sf"
	"github.com/goceleris/celeris/middleware/store"
)

// ErrNotSupported is returned by [InvalidatePrefix] when the given
// store does not implement the required extension interface.
var ErrNotSupported = errors.New("cache: store does not support this operation")

// New returns a cache middleware.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)

	// Pre-lowercase the response header name once so c.SetHeader's
	// fast-path fires on HIT/MISS/ERROR writes — the default "X-Cache"
	// has uppercase and otherwise allocates per response.
	cfg.HeaderName = strings.ToLower(cfg.HeaderName)

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	methods := make(map[string]struct{}, len(cfg.Methods))
	for _, m := range cfg.Methods {
		methods[strings.ToUpper(m)] = struct{}{}
	}

	include := make(map[string]struct{}, len(cfg.IncludeHeaders))
	for _, h := range cfg.IncludeHeaders {
		include[strings.ToLower(h)] = struct{}{}
	}
	exclude := make(map[string]struct{}, len(cfg.ExcludeHeaders))
	for _, h := range cfg.ExcludeHeaders {
		exclude[strings.ToLower(h)] = struct{}{}
	}

	keyGen := cfg.KeyGenerator
	if keyGen == nil {
		keyGen = defaultKeyGenerator(cfg.VaryHeaders)
	}

	group := sf.New[[]byte]()

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}
		if _, ok := methods[strings.ToUpper(c.Method())]; !ok {
			return c.Next()
		}

		key := keyGen(c)
		ctx := c.Context()

		// Attempt hit.
		if raw, err := cfg.Store.Get(ctx, key); err == nil {
			if rep, derr := store.DecodeResponse(raw); derr == nil {
				// Skip remaining handlers; cached response is authoritative.
				c.Abort()
				return replay(c, cfg, rep)
			}
			_ = cfg.Store.Delete(ctx, key)
		} else if !errors.Is(err, store.ErrNotFound) {
			// Store transport error — skip caching for this request.
			if cfg.HeaderName != "" {
				c.SetHeader(cfg.HeaderName, "ERROR")
			}
			return c.Next()
		}

		if cfg.DisableSingleflight {
			return executeAndStore(c, cfg, include, exclude, key)
		}

		// Singleflight: only the leader runs the handler. Followers decode
		// the leader's encoded bytes and replay on their own Context.
		//
		// The coalesced call only computes: it is over once the handler has
		// run and its response is encoded. Every request writes its own
		// response after the call, the leader included. A write waits for
		// its own client, and a client that reads slowly (an HTTP/2 peer
		// that grants no window) must not hold every follower of the key,
		// nor, on epoll and io_uring, the event loop each sync follower runs
		// on; nor may that client's write error become the followers' result
		// (celeris#913). The leader stores the response after the followers
		// are released, while the key is still held, so a Set that hangs (a
		// remote store) holds no follower either, and a request that comes
		// during the Set, within the response's TTL, takes the response from
		// the call (celeris#921).
		var ttl time.Duration
		encoded, leader, err := group.Do(ctx, key, func() ([]byte, time.Duration, error) {
			// nil dst: leader returns the encoded bytes via sf.Do; followers
			// may still be reading them after this call frame returns, so
			// we can't share a pooled buffer here. The response is handed
			// to a request that arrives during the Set only within its TTL,
			// as the stored entry would be: a Set that hangs must not serve
			// it for longer.
			enc, effectiveTTL, chainErr := capture(c, cfg, include, exclude, nil)
			ttl = effectiveTTL
			return enc, effectiveTTL, chainErr
		}, func(enc []byte, _ error) {
			if enc != nil {
				_ = cfg.Store.Set(ctx, key, enc, ttl)
			}
		})
		if leader {
			return writeMiss(c, cfg, err)
		}
		if errors.Is(err, sf.ErrLeaderPanicked) {
			// The leader's handler (or its Set) panicked; the panic is the
			// leader's. This request runs its own handler, as it does when
			// the leader's response is not cacheable (celeris#921).
			return executeAndStore(c, cfg, include, exclude, key)
		}
		if err != nil {
			return err
		}
		// Follower path.
		if encoded == nil {
			// Leader determined the response is not cacheable. Followers
			// fall back to running their own handler.
			return executeAndStore(c, cfg, include, exclude, key)
		}
		rep, derr := store.DecodeResponse(encoded)
		if derr != nil {
			return executeAndStore(c, cfg, include, exclude, key)
		}
		c.Abort()
		return replay(c, cfg, rep)
	}
}

// executeAndStore runs the handler, stores the encoded bytes on cache
// eligibility, then writes the response to the wire with the MISS
// header. Used on the non-singleflight path and by followers that fall
// back (leader produced no cacheable bytes). The store comes first, so
// a client that reads slowly does not hold back the fill.
//
// Borrows the encode buffer from cacheBufPool — Store.Set copies its
// input internally (MemoryKV.Set reuses the existing backing array per
// R66; redis/memcached adapters write through the wire), so the pooled
// buffer is safe to recycle the moment Set returns.
func executeAndStore(c *celeris.Context, cfg Config, include, exclude map[string]struct{}, key string) error {
	bufPtr := cacheBufPool.Get().(*[]byte)
	encoded, ttl, chainErr := capture(c, cfg, include, exclude, (*bufPtr)[:0])
	if encoded != nil {
		_ = cfg.Store.Set(c.Context(), key, encoded, ttl)
	}
	if cap(encoded) <= cacheBufMaxPooled {
		*bufPtr = encoded
	} else {
		*bufPtr = (*bufPtr)[:0]
	}
	cacheBufPool.Put(bufPtr)
	return writeMiss(c, cfg, chainErr)
}

// cacheBufPool recycles encode-buffer backing arrays across
// non-singleflight MISS requests. cacheBufMaxPooled caps retained
// capacity so a pathological oversized response doesn't bloat the pool.
const cacheBufMaxPooled = 64 * 1024

var cacheBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// capture buffers + runs the remaining handler chain on c and, iff the
// response is cacheable, returns its encoding and the TTL to store it with;
// the buffered response is left for [writeMiss] to put on the wire. The
// effective TTL is min(cfg.TTL, Cache-Control max-age) unless
// IgnoreCacheControl is set. Returns nil bytes for ineligible responses
// (status filter, size cap, no-store/private, etc.), and the chain's error,
// if any. The caller stores the bytes.
//
// dst is an optional pre-allocated buffer to encode into. Pass nil for
// the singleflight leader (followers hold onto the returned bytes past
// the caller's stack frame), or a pooled buffer on the non-singleflight
// path where Store.Set's internal copy lets us recycle immediately.
func capture(c *celeris.Context, cfg Config, include, exclude map[string]struct{}, dst []byte) ([]byte, time.Duration, error) {
	c.BufferResponse()
	chainErr := c.Next()
	status := c.ResponseStatus()
	body := c.ResponseBody()
	respHeaders := c.ResponseHeaders()

	cacheBytes := dst
	effectiveTTL := cfg.TTL
	if chainErr == nil && !answersRange(status) && cfg.StatusFilter(status) && len(body) <= cfg.MaxBodyBytes {
		cacheable := true
		if !cfg.IgnoreCacheControl {
			for _, h := range respHeaders {
				// c.SetHeader lowercases keys on storage, so an exact ==
				// is both correct and ~10x faster than strings.EqualFold.
				if h[0] == "cache-control" {
					v := strings.ToLower(h[1])
					if strings.Contains(v, "no-store") || strings.Contains(v, "private") {
						cacheable = false
						break
					}
					if secs, ok := parseMaxAge(v); ok {
						if d := time.Duration(secs) * time.Second; d > 0 && d < effectiveTTL {
							effectiveTTL = d
						}
					}
				}
			}
		}
		if cacheable {
			filtered := filterHeaders(respHeaders, include, exclude)
			enc := store.EncodedResponse{Status: status, Headers: filtered, Body: body}
			cacheBytes = enc.AppendTo(cacheBytes[:0])
		} else {
			cacheBytes = nil
		}
	} else {
		cacheBytes = nil
	}
	return cacheBytes, effectiveTTL, chainErr
}

// writeMiss puts c's buffered response on the wire with the MISS header.
// It returns the write's error, or else chainErr: the handler's error,
// which the router answers.
func writeMiss(c *celeris.Context, cfg Config, chainErr error) error {
	// Set MISS header before flushing so it makes it to the wire.
	if cfg.HeaderName != "" {
		c.SetHeader(cfg.HeaderName, "MISS")
	}
	if ferr := c.FlushResponse(); ferr != nil {
		return ferr
	}
	return chainErr
}

// answersRange reports a status that answers the request's Range header: a
// 206 Partial Content carries one part, a 416 says that Range cannot be
// satisfied. Neither is stored, whatever StatusFilter says: the default key
// does not include Range, so a later request without it (or with another)
// would get the part, or the 416, back (celeris#832). They are not stored
// when a key does include it either (VaryHeaders, a KeyGenerator): a replay
// skips the handler's If-Range check, which can make the same Range get the
// whole representation.
func answersRange(status int) bool {
	return status == 206 || status == 416
}

func replay(c *celeris.Context, cfg Config, rep store.EncodedResponse) error {
	ct := "application/octet-stream"
	for _, h := range rep.Headers {
		// Stored headers come from c.ResponseHeaders() which is already
		// lowercased by celeris.Context.SetHeader (HTTP/2 RFC 7540 §8.1.2),
		// so a plain string compare is correct and ~10x faster than
		// strings.EqualFold on every HIT.
		if h[0] == "content-type" {
			ct = h[1]
			continue
		}
		c.SetHeader(h[0], h[1])
	}
	if cfg.HeaderName != "" {
		c.SetHeader(cfg.HeaderName, "HIT")
	}
	return c.Blob(rep.Status, ct, rep.Body)
}

func filterHeaders(hs [][2]string, include, exclude map[string]struct{}) [][2]string {
	out := make([][2]string, 0, len(hs))
	for _, h := range hs {
		l := strings.ToLower(h[0])
		if len(include) > 0 {
			if _, ok := include[l]; !ok {
				continue
			}
		}
		if _, ok := exclude[l]; ok {
			continue
		}
		out = append(out, h)
	}
	return out
}

func parseMaxAge(cc string) (int, bool) {
	const tok = "max-age="
	idx := strings.Index(cc, tok)
	if idx < 0 {
		return 0, false
	}
	rest := cc[idx+len(tok):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

func defaultKeyGenerator(vary []string) func(*celeris.Context) string {
	varyLower := make([]string, len(vary))
	for i, h := range vary {
		varyLower[i] = strings.ToLower(h)
	}
	return func(c *celeris.Context) string {
		m := c.Method()
		p := c.Path()
		rq := c.RawQuery()
		// Fast path: plain "METHOD PATH" when there's no query and no
		// vary headers to mix in. Covers the common REST-API cache key
		// shape with one concat alloc instead of strings.Builder
		// grow-then-materialize.
		if rq == "" && len(varyLower) == 0 {
			return m + " " + p
		}
		var b strings.Builder
		// Pre-size the builder so WriteString doesn't realloc on the
		// common mid-size key. Estimate: method + ' ' + path + '?' +
		// query + (|h=v) for each vary.
		n := len(m) + 1 + len(p)
		if rq != "" {
			n += 1 + len(rq)
		}
		for _, h := range varyLower {
			n += 2 + len(h) + len(c.Header(h))
		}
		b.Grow(n)
		b.WriteString(m)
		b.WriteString(" ")
		b.WriteString(p)
		if rq != "" {
			b.WriteString("?")
			b.WriteString(sortedQuery(rq))
		}
		for _, h := range varyLower {
			b.WriteString("|")
			b.WriteString(h)
			b.WriteString("=")
			b.WriteString(c.Header(h))
		}
		return b.String()
	}
}

func sortedQuery(rq string) string {
	// Single-param fast path: no split/sort/join allocations.
	if strings.IndexByte(rq, '&') < 0 {
		return rq
	}
	// Small-query fast path: up to 8 parameters fit in a stack array
	// and avoid the Split heap allocation. If the parts are already
	// sorted we return rq unchanged (second-level skip — many clients
	// send canonical query strings already).
	var stackBuf [8]string
	parts := stackBuf[:0]
	start := 0
	for i := 0; i < len(rq); i++ {
		if rq[i] == '&' {
			if len(parts) == cap(stackBuf) {
				parts = nil
				break
			}
			parts = append(parts, rq[start:i])
			start = i + 1
		}
	}
	if parts != nil {
		if len(parts) == cap(stackBuf) {
			parts = nil
		} else {
			parts = append(parts, rq[start:])
		}
	}
	if parts != nil {
		sorted := true
		for i := 1; i < len(parts); i++ {
			if parts[i-1] > parts[i] {
				sorted = false
				break
			}
		}
		if sorted {
			return rq
		}
		sort.Strings(parts)
		return strings.Join(parts, "&")
	}
	// Fallback for >8 params.
	fparts := strings.Split(rq, "&")
	sort.Strings(fparts)
	return strings.Join(fparts, "&")
}

// Invalidate removes the exact cache entry for the given key.
func Invalidate(s store.KV, key string) error {
	if s == nil {
		return ErrNotSupported
	}
	return s.Delete(context.Background(), key)
}

// InvalidatePrefix removes every cache entry whose key begins with
// prefix. Returns [ErrNotSupported] if the store does not implement
// [store.PrefixDeleter].
func InvalidatePrefix(s store.KV, prefix string) error {
	pd, ok := s.(store.PrefixDeleter)
	if !ok {
		return ErrNotSupported
	}
	return pd.DeletePrefix(context.Background(), prefix)
}
