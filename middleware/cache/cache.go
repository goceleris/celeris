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
	"sync"
	"time"

	"github.com/goceleris/celeris"
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

	sf := newSingleflight()

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
		res, leader, err := sf.do(key, func() (sfResult, error) {
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
				if strings.EqualFold(h[0], "cache-control") {
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
		if strings.EqualFold(h[0], "content-type") {
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
		var b strings.Builder
		b.WriteString(c.Method())
		b.WriteString(" ")
		b.WriteString(c.Path())
		if rq := c.RawQuery(); rq != "" {
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
	parts := strings.Split(rq, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
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

// sfGroup coalesces concurrent in-flight misses for the same key. The
// leader runs the producer; followers block on the leader's result.
type sfGroup struct {
	mu    sync.Mutex
	calls map[string]*sfCall
}

type sfCall struct {
	wg     sync.WaitGroup
	result sfResult
	err    error
}

func newSingleflight() *sfGroup {
	return &sfGroup{calls: make(map[string]*sfCall)}
}

func (g *sfGroup) do(key string, fn func() (sfResult, error)) (sfResult, bool, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.result, false, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	result, err := fn()
	c.result = result
	c.err = err
	return result, true, err
}
