package singleflight

import (
	"net/url"
	"slices"
	"strings"

	"github.com/goceleris/celeris"
)

// Config defines the singleflight middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// KeyFunc extracts the deduplication key from the request. Requests
	// with the same key that arrive while a leader request is in-flight
	// are coalesced — waiters receive a copy of the leader's response.
	//
	// Default: method + "\x00" + path + "\x00q=" + sorted-query-string,
	// then the Authorization, Cookie and Accept-Encoding headers and the
	// four preconditions (If-None-Match, If-Modified-Since, If-Match and
	// If-Unmodified-Since). The query and each header are labelled, so one
	// does not read as another as long as no value holds a NUL byte. HTTP/1
	// header values cannot, but the native engines do not check the request
	// target (HTTP/1) or field values and :path (HTTP/2) for control bytes,
	// so a path with a NUL can read as labelled components of another
	// request's key, as it does in the key of earlier releases.
	// The Authorization and Cookie components ensure that requests from
	// different authenticated users produce different keys, preventing
	// cross-user data leakage. Unauthenticated requests (no auth/cookie
	// headers) still coalesce normally. Accept-Encoding keeps requests that
	// accept different encodings apart when compress runs inside
	// singleflight. Two values are one component when the negotiator reads
	// them alike (as c.AcceptsEncodings does: "gzip, deflate" and
	// "gzip,deflate", "GZIP" and "gzip"), and the order is kept; "gzip;q=0"
	// and "gzip;Q=0", which it reads alike, are one, and "gzip;q=0" and
	// "gzip;q = 0", which it tells apart, are two. A value longer than 256
	// bytes is held as sent, not read, so two spellings of one long list are
	// two components (only a missed coalescing). The preconditions keep
	// requests with different validators apart, because the response answers
	// its own validator (a 304 or a 412), and let identical revalidations (a
	// CDN or browsers sending the same If-None-Match) coalesce. That holds
	// for GET and HEAD only: a request of another method that carries a
	// precondition (a PUT with If-Match) is never coalesced, because its
	// handler is the update, and a request that took another's response
	// would lose it.
	//
	// If you provide a custom KeyFunc, ensure it incorporates user
	// identity for any endpoint that returns user-specific data. A request
	// that carries a precondition is never coalesced under a custom key,
	// because the key may not include the validator; a Range request never
	// is under any key. Whatever the key, a waiter whose request differs
	// from the leader's in a header the leader's response names in Vary (or
	// a response with Vary: *) runs its own handler instead of taking the
	// leader's response; Accept-Encoding values are compared as the default
	// key does.
	KeyFunc func(c *celeris.Context) string
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaultKeyFunc
	}
	return cfg
}

// No validation needed: all Config fields have safe zero values.
func (cfg Config) validate() {}

// keyValidators are the request headers, besides Authorization, Cookie and
// Accept-Encoding, that the default key holds: the preconditions. A conditional
// request's response answers its own validator (a 304 or a 412), so the key
// keeps requests with different validators apart, and requests that send the
// same one still coalesce (celeris#888 item 23).
var keyValidators = [...]struct{ header, label string }{
	{"if-none-match", "\x00nm="},
	{"if-modified-since", "\x00ms="},
	{"if-match", "\x00ma="},
	{"if-unmodified-since", "\x00us="},
}

func defaultKeyFunc(c *celeris.Context) string {
	key, _ := defaultKey(c)
	return key
}

// defaultKey is the default key, and whether the request carries one of the
// four preconditions. The middleware needs both and the key reads the
// preconditions anyway, so one pass over the headers serves the key and the
// bypass of a conditional write. For a conditional write (not GET or HEAD:
// see safeToCoalesce) the key is "": the middleware does not coalesce it, and
// the preconditions are read before anything is built.
func defaultKey(c *celeris.Context) (key string, conditional bool) {
	m := c.Method()
	var val [len(keyValidators)]string
	for i, v := range keyValidators {
		val[i] = c.Header(v.header)
		if val[i] != "" {
			conditional = true
		}
	}
	if conditional && !safeToCoalesce(m) {
		// A conditional write is not coalesced (the caller bypasses it), so
		// its key is not built: no query sort, no canonical form, no copy.
		return "", true
	}
	p := c.Path()
	rq := c.RawQuery()
	auth := c.Header("authorization")
	cookie := c.Header("cookie")
	// The encoding compress negotiates comes from Accept-Encoding, so
	// requests that accept different encodings get different responses
	// (celeris#912). Equivalent values (blanks, case) are one key part.
	// A value past maxCanonicalAE is held as sent, copied once, into the key.
	var aeBuf [64]byte
	var ae []byte
	aeLong := c.Header("accept-encoding")
	if len(aeLong) <= maxCanonicalAE {
		ae = canonicalAppend(aeBuf[:0], "accept-encoding", aeLong)
		aeLong = ""
	}
	var q string
	if rq != "" {
		// Clone query params before sorting to avoid mutating the
		// context's cached queryCache.
		params := c.QueryParams()
		sorted := make(url.Values, len(params))
		for k, v := range params {
			cp := make([]string, len(v))
			copy(cp, v)
			slices.Sort(cp)
			sorted[k] = cp
		}
		q = sorted.Encode()
	}
	// Each header component is labelled, so one header's value does not
	// read as another's (unless a value holds a NUL: see Config.KeyFunc).
	// The key is built in one allocation.
	n := len(m) + 1 + len(p)
	if q != "" {
		n += 3 + len(q)
	}
	for _, v := range [...]string{auth, cookie} {
		if v != "" {
			n += 3 + len(v)
		}
	}
	if len(ae) > 0 || aeLong != "" {
		n += 3 + len(ae) + len(aeLong)
	}
	for i, v := range val {
		if v != "" {
			n += len(keyValidators[i].label) + len(v)
		}
	}
	if n == len(m)+1+len(p) {
		return m + "\x00" + p, false
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(m)
	b.WriteString("\x00")
	b.WriteString(p)
	if q != "" {
		b.WriteString("\x00q=")
		b.WriteString(q)
	}
	if auth != "" {
		b.WriteString("\x00a=")
		b.WriteString(auth)
	}
	if cookie != "" {
		b.WriteString("\x00c=")
		b.WriteString(cookie)
	}
	if len(ae) > 0 || aeLong != "" {
		b.WriteString("\x00e=")
		b.Write(ae)
		b.WriteString(aeLong)
	}
	for i, v := range val {
		if v != "" {
			b.WriteString(keyValidators[i].label)
			b.WriteString(v)
		}
	}
	return b.String(), conditional
}
