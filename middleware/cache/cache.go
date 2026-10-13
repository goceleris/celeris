// Package-level documentation lives in doc.go.

package cache

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/internal/sf"
	"github.com/goceleris/celeris/middleware/store"
)

// genBase is the origin of generations: time.Since reads the monotonic clock,
// which a wall-clock step does not move, and, unlike a shared counter, costs
// no cache-line transfer between cores on a busy miss path.
var genBase = time.Now()

// genNow returns the generation of a fill or a write that starts now: the
// nanoseconds since genBase, plus one so that no generation is 0 (an entry
// written by a plain Set has none). Two starts in the same clock tick share a
// generation, and then the later write wins.
func genNow() uint64 { return uint64(time.Since(genBase)) + 1 }

// storeFill writes a fill's encoded response. The cache's own *MemoryStore
// applies it in order: setIfNewer stores the value unless the key holds a live
// entry that a fill of a newer generation wrote. A generation is the time a
// fill's handler started (see genNow), so a fill that started later, which
// read the origin later, is not overwritten by an earlier one whose write
// lands after it: two concurrent misses with DisableSingleflight, or a
// coalesced call that was replaced once its result expired while its Set hung.
// Every other store gets a plain Set, whatever it embeds: a type that embeds
// *MemoryStore and overrides Set (metrics, a write-through, compression) must
// see its Set called, and an interface test would be satisfied by the
// unexported method that embedding promotes. The order of two writes that
// overlap is then the order that store applies them in (celeris#888 item 30).
func storeFill(ctx context.Context, kv store.KV, key string, enc []byte, ttl time.Duration, gen uint64) error {
	if ms, ok := kv.(*MemoryStore); ok {
		return ms.setIfNewer(ctx, key, enc, ttl, gen)
	}
	return kv.Set(ctx, key, enc, ttl)
}

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
		var gen uint64
		encoded, leader, err := group.Do(ctx, key, func() ([]byte, time.Duration, error) {
			// nil dst: leader returns the encoded bytes via sf.Do; followers
			// may still be reading them after this call frame returns, so
			// we can't share a pooled buffer here. The response is handed
			// to a request that arrives during the Set only within its TTL,
			// as the stored entry would be: a Set that hangs must not serve
			// it for longer.
			gen = genNow()
			enc, effectiveTTL, chainErr := capture(c, cfg, include, exclude, nil)
			ttl = effectiveTTL
			return enc, effectiveTTL, chainErr
		}, func(enc []byte, _ error) {
			if enc != nil {
				_ = storeFill(ctx, cfg.Store, key, enc, ttl, gen)
			}
		})
		if leader {
			return writeMiss(c, cfg, err)
		}
		if errors.Is(err, sf.ErrLeaderPanicked) || errors.Is(err, sf.ErrLeaderCanceled) {
			// The leader's handler (or its Set) panicked; the panic is the
			// leader's. Or the leader's own request context ended (its
			// client went away) and the handler said so; that error is its
			// client's, not the key's. This request runs its own handler, as
			// it does when the leader's response is not cacheable
			// (celeris#921, celeris#888 item 26).
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
	gen := genNow()
	encoded, ttl, chainErr := capture(c, cfg, include, exclude, (*bufPtr)[:0])
	if encoded != nil {
		_ = storeFill(c.Context(), cfg.Store, key, encoded, ttl, gen)
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
					noStore, secs, hasMaxAge := parseCacheControl(strings.ToLower(h[1]))
					if noStore {
						cacheable = false
						break
					}
					// secs is > 0 here: max-age=0 is a reason not to store.
					if hasMaxAge {
						if d := time.Duration(secs) * time.Second; d < effectiveTTL {
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

// maxDeltaSeconds caps a max-age: math.MaxInt32 seconds is about 68 years, and
// as a Duration it is far from overflowing, where max-age=10000000000 times
// time.Second wraps to a negative TTL, which the stores read as no expiry.
const maxDeltaSeconds = math.MaxInt32

// parseCacheControl reads a lowercased Cache-Control value as directive
// tokens. noStore reports that the response must not be stored: no-store and
// private, as before, and now no-cache (a stored response must be revalidated
// before every reuse, which this cache does not do; the field-qualified form
// no-cache="x" is treated the same, as the cautious reading) and max-age=0
// (stale the moment it is made). hasMaxAge and maxAge give the first
// max-age directive, at most maxDeltaSeconds (RFC 9111
// §1.2.2: a larger delta-seconds counts as the greatest value the cache can
// represent), so that maxAge seconds always fits a time.Duration. The
// directives are matched as tokens, so x-max-age=0 is not max-age=0 and
// no-cache="x" is no-cache, and the storage decision and the TTL come from
// this one parse (celeris#888 item 27).
//
// Two rules make this never looser than main, which refused any value that held
// "private" or "no-store" anywhere. The floor: a value that holds either word,
// anywhere, is not stored, whatever the parse below would read (a word glued to
// other text, as when SetHeader strips a CR, LF or NUL between two directives,
// x-private-ext, and one inside a quoted string included); nothing below can
// undo it, the parse can only add refusals. And a max-age whose argument is not
// 1*DIGIT (RFC 9111 §4.2.1: an invalid max-age is stale) is not stored, so
// max-age=60abc is not max-age=60, and that includes "max-age=", abc, -1 and a
// quoted string.
//
// A value that is not a list of directives (RFC 9111 §5.2: directive = token
// ["=" (token / quoted-string)], separated by commas, RFC 9110 §5.6.1) is not
// stored either, because what a recipient reads in it is a guess, and a wrong
// guess serves one user's response to another. It is such a value when: the
// name is empty or holds a byte that is not a token character (so "private; max-age"
// is not private with a max-age, and a blank before the '=' is not accepted);
// an unquoted value holds one (so "max-age=60 private" is not max-age=60); or a
// quote cannot be read around: a string that never closes, a double quote
// where a string cannot start, anything but blanks after the closing quote
// (nextDirective). Empty list elements (",,") and blanks around the commas are
// part of the list syntax, and "name=" is accepted as before for the names other
// than max-age.
func parseCacheControl(cc string) (noStore bool, maxAge int, hasMaxAge bool) {
	// The floor, before any parsing: main's check (cc is lowercased), which
	// nothing the parse below reads can undo.
	if strings.Contains(cc, "private") || strings.Contains(cc, "no-store") {
		return true, 0, false
	}
	for len(cc) > 0 {
		var d string
		var doubtful bool
		d, cc, doubtful = nextDirective(cc)
		if doubtful {
			// A quote the directives cannot be read around: the ones after
			// it may be hidden, so the response is not stored.
			return true, 0, false
		}
		d = strings.Trim(d, " \t")
		if d == "" {
			continue // an empty list element
		}
		name, val, hasVal := strings.Cut(d, "=")
		if !isToken(name) || hasVal && !isQuoted(val) && !isTokenOrEmpty(val) {
			return true, 0, false
		}
		switch name {
		case "no-store", "private", "no-cache":
			return true, 0, false
		case "max-age":
			// RFC 9111 §4.2.1: an invalid max-age is stale, and this cache
			// stores no stale response. The argument is 1*DIGIT, nothing else
			// (empty, abc, -1, 60abc, a quoted string), whichever max-age it is.
			n, ok := deltaSeconds(val)
			if !ok || n == 0 {
				return true, 0, false
			}
			if !hasMaxAge {
				maxAge, hasMaxAge = min(n, maxDeltaSeconds), true
			}
		}
	}
	return false, maxAge, hasMaxAge
}

// isToken reports a non-empty token (RFC 9110 §5.6.2): every byte a tchar.
func isToken(s string) bool { return s != "" && isTokenOrEmpty(s) }

func isTokenOrEmpty(s string) bool {
	for i := 0; i < len(s); i++ {
		if !tchars[s[i]] {
			return false
		}
	}
	return true
}

// tchars marks RFC 9110 §5.6.2's tchar bytes.
var tchars = func() (t [256]bool) {
	for _, r := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~" {
		t[r] = true
	}
	return
}()

// isQuoted reports a value that is a quoted string. nextDirective has seen to
// it that the string closes and that only blanks follow it.
func isQuoted(val string) bool { return val != "" && val[0] == '"' }

// nextDirective splits the first comma-separated directive off a
// Cache-Control value; a comma inside a quoted string (no-cache="a, b") does
// not end it. A quoted string starts only at the double quote that directly
// follows the directive's first "=" (RFC 9111 5.2: directive = token ["="
// (token / quoted-string)]), and inside a string a backslash escapes the byte
// after it (RFC 9110 5.6.4: ext="a\"" is one string), so a quoted-pair does
// not end a string early and a string does not start anywhere else.
// doubtful reports a value this cannot read with certainty: a string still
// open at the end of the value, a double quote anywhere but where a string
// starts (ext=a"b, ext=a="b, private", "private"), and anything but blanks
// between the closing quote and the comma or the end (x="a"private). The
// directives after such a quote may be hidden in a string or cut by one, so the
// caller must not trust the rest of the value.
func nextDirective(cc string) (directive, rest string, doubtful bool) {
	inQuote, closed := false, false
	eq := -2 // the index of the directive's first "=", -2 until it is seen
	for i := 0; i < len(cc); i++ {
		switch {
		case inQuote:
			switch cc[i] {
			case '\\':
				i++
			case '"':
				inQuote, closed = false, true
			}
		case closed && cc[i] != ',' && cc[i] != ' ' && cc[i] != '\t':
			return cc, "", true
		case cc[i] == '"':
			if eq < 0 || i != eq+1 {
				return cc, "", true
			}
			inQuote = true
		case cc[i] == '=' && eq < 0:
			eq = i
		case cc[i] == ',':
			return cc[:i], cc[i+1:], false
		}
	}
	return cc, "", inQuote
}

// deltaSeconds parses v as delta-seconds (RFC 9111 §1.2.2: 1*DIGIT). ok is
// false for anything else, an empty v included. A number too large for an int
// is the greatest value, as the RFC has it.
func deltaSeconds(v string) (n int, ok bool) {
	if v == "" {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return math.MaxInt32, true // 1*DIGIT, too large for an int
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
