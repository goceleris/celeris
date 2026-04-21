// Package cache provides an HTTP response cache middleware for celeris.
//
// The middleware buffers the handler-produced response (status, headers,
// body), encodes it via the versioned wire format in [middleware/store],
// and persists it under a user-derived key. Subsequent matching requests
// replay the stored response without re-running the handler chain.
//
// Concurrent misses for the same key are coalesced via singleflight when
// enabled (default), so only one handler invocation runs per miss.
// Cache-Control directives on the response (no-store, private, max-age)
// are honored by default — opt out via [Config.RespectCacheControl].
package cache

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
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

	group := sf.New[sfResult]()

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

		if !cfg.Singleflight {
			return executeAndStore(c, cfg, include, exclude, key)
		}

		// Singleflight: only the leader runs the handler. Followers decode
		// the leader's encoded bytes and replay on their own Context.
		// sfResult carries the encoded bytes plus the effective TTL the
		// leader computed so followers don't re-derive it (and so the
		// leader can apply per-response Cache-Control max-age caps).
		res, leader, err := group.Do(key, func() (sfResult, error) {
			buf, ttl, rerr := executeAndReturnWithTTL(c, cfg, include, exclude)
			return sfResult{bytes: buf, ttl: ttl}, rerr
		})
		if err != nil {
			return err
		}
		if leader {
			if res.bytes != nil {
				_ = cfg.Store.Set(ctx, key, res.bytes, res.ttl)
			}
			return nil
		}
		// Follower path.
		if res.bytes == nil {
			// Leader determined the response is not cacheable. Followers
			// fall back to running their own handler.
			return executeAndStore(c, cfg, include, exclude, key)
		}
		rep, derr := store.DecodeResponse(res.bytes)
		if derr != nil {
			return executeAndStore(c, cfg, include, exclude, key)
		}
		c.Abort()
		return replay(c, cfg, rep)
	}
}

// sfResult bundles the encoded response bytes and the effective TTL
// (possibly capped by Cache-Control max-age) so the singleflight
// leader can pass both to its waiters and the store.
type sfResult struct {
	bytes []byte
	ttl   time.Duration
}

// executeAndStore runs the handler, flushes its response to the wire
// with the MISS header, and stores the encoded bytes on cache
// eligibility. Used on the non-singleflight path and by followers that
// fall back (leader produced no cacheable bytes).
func executeAndStore(c *celeris.Context, cfg Config, include, exclude map[string]struct{}, key string) error {
	bytes, ttl, err := executeAndReturnWithTTL(c, cfg, include, exclude)
	if err != nil {
		return err
	}
	if bytes != nil {
		_ = cfg.Store.Set(c.Context(), key, bytes, ttl)
	}
	return nil
}

// executeAndReturnWithTTL buffers + runs the remaining handler chain
// on c, flushes the wire with the MISS header set, and returns the
// (encoded-bytes, effective-ttl) iff the response is cacheable.
// Effective TTL is min(cfg.TTL, Cache-Control max-age) when
// RespectCacheControl is enabled. Returns (nil, 0, nil) for ineligible
// responses (status filter, size cap, no-store/private, etc.).
func executeAndReturnWithTTL(c *celeris.Context, cfg Config, include, exclude map[string]struct{}) ([]byte, time.Duration, error) {
	c.BufferResponse()
	chainErr := c.Next()
	status := c.ResponseStatus()
	body := c.ResponseBody()
	respHeaders := c.ResponseHeaders()

	var cacheBytes []byte
	effectiveTTL := cfg.TTL
	if chainErr == nil && cfg.StatusFilter(status) && len(body) <= cfg.MaxBodyBytes {
		cacheable := true
		if cfg.RespectCacheControl {
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
			cacheBytes = enc.Encode()
		}
	}

	// Set MISS header before flushing so it makes it to the wire.
	if cfg.HeaderName != "" {
		c.SetHeader(cfg.HeaderName, "MISS")
	}
	if ferr := c.FlushResponse(); ferr != nil {
		return cacheBytes, effectiveTTL, ferr
	}
	if chainErr != nil {
		return nil, 0, chainErr
	}
	return cacheBytes, effectiveTTL, nil
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
