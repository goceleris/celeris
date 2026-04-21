// Package extract provides a shared token/value extractor for middleware that
// reads values from request headers, query parameters, cookies, form fields,
// or path parameters using a "source:name[:prefix]" lookup string format.
package extract

import (
	"net/url"
	"strings"

	"github.com/goceleris/celeris"
)

// Func extracts a string value from a request context.
type Func func(c *celeris.Context) string

// Parse parses a comma-separated lookup string into an extractor function.
// Format: "source:name[:prefix]" where source is header, query, cookie, form, or param.
// Multiple sources separated by commas are tried in order; the first non-empty result wins.
func Parse(lookup string) Func {
	parts := strings.Split(lookup, ",")
	if len(parts) == 1 {
		return parseSingle(strings.TrimLeft(parts[0], " \t"))
	}
	extractors := make([]Func, 0, len(parts))
	for _, p := range parts {
		extractors = append(extractors, parseSingle(strings.TrimLeft(p, " \t")))
	}
	return func(c *celeris.Context) string {
		for _, fn := range extractors {
			if v := fn(c); v != "" {
				return v
			}
		}
		return ""
	}
}

func parseSingle(lookup string) Func {
	parts := strings.SplitN(lookup, ":", 3)
	if len(parts) < 2 {
		panic("extract: invalid lookup format: " + lookup)
	}
	source := strings.TrimSpace(parts[0])
	name := strings.TrimSpace(parts[1])
	prefix := ""
	if len(parts) == 3 {
		prefix = parts[2]
	}

	switch source {
	case "header":
		name = strings.ToLower(name)
		return func(c *celeris.Context) string {
			v := c.Header(name)
			if prefix != "" {
				if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
					return v[len(prefix):]
				}
				return ""
			}
			return v
		}
	case "query":
		return func(c *celeris.Context) string {
			return queryLookup(c.RawQuery(), name)
		}
	case "cookie":
		return func(c *celeris.Context) string {
			v, _ := c.Cookie(name)
			return v
		}
	case "form":
		return func(c *celeris.Context) string {
			return c.FormValue(name)
		}
	case "param":
		return func(c *celeris.Context) string {
			return c.Param(name)
		}
	default:
		panic("extract: unsupported source: " + source)
	}
}

// queryLookup scans raw for key=value pairs and returns the first value
// matching key, without building a url.Values map. The common case (ASCII
// key + unescaped value, e.g. a JWT in a ?token=... query) returns a
// substring of raw — zero alloc. Encoded keys/values fall back to
// url.QueryUnescape. Mirrors url.ParseQuery's first-match semantics.
func queryLookup(raw, key string) string {
	for len(raw) > 0 {
		var kv string
		if i := strings.IndexByte(raw, '&'); i >= 0 {
			kv = raw[:i]
			raw = raw[i+1:]
		} else {
			kv = raw
			raw = ""
		}
		if kv == "" {
			continue
		}
		var k, v string
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			k, v = kv[:eq], kv[eq+1:]
		} else {
			k = kv
		}
		if k == key {
			return unescapeValue(v)
		}
		if strings.IndexByte(k, '%') >= 0 || strings.IndexByte(k, '+') >= 0 {
			if uk, err := url.QueryUnescape(k); err == nil && uk == key {
				return unescapeValue(v)
			}
		}
	}
	return ""
}

func unescapeValue(v string) string {
	if v == "" {
		return ""
	}
	if strings.IndexByte(v, '%') < 0 && strings.IndexByte(v, '+') < 0 {
		return v
	}
	if u, err := url.QueryUnescape(v); err == nil {
		return u
	}
	return ""
}
