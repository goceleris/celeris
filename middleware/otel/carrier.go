package otel

import (
	"slices"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	"github.com/goceleris/celeris"
)

// Compile-time interface check.
var _ propagation.TextMapCarrier = headerCarrier{}

// headerCarrier adapts a celeris Context to propagation.TextMapCarrier for
// server-side instrumentation.
//
// Get reads from request headers (used by Extract to parse incoming trace
// context from the caller). Set writes to response headers (used by Inject
// to propagate trace context back to the caller). This asymmetry is the
// correct behavior for HTTP server middleware: the server extracts context
// from the inbound request and injects it into the outbound response.
//
// Get returns a copy of every value but one, and Keys returns copies. On
// epoll and io_uring a request header is a view of the engine's receive
// buffer: safe to compare while the request is handled, unsafe to keep after
// it. Propagators keep what they read, in the context the middleware installs
// with SetContext (trace.ParseTraceState and baggage.Parse return substrings
// of their input, so a tracestate or baggage value outlives the request in
// the span context it builds, and a sampled span's context lives until the
// exporter has sent it; a propagator that walks Keys may keep the names it
// matches), and that context outlives the request whenever something keeps
// it: a detached WebSocket or SSE stream derives its context from it
// (celeris#714). A header that is absent costs nothing; one that is present
// costs one allocation of its size.
//
// The exception is "traceparent": the W3C propagator decodes it into the
// fixed-size trace and span IDs and keeps no part of the string, so Get
// returns the request's view of it and the request pays no allocation for
// it. A propagator that keeps a substring of "traceparent" (a custom one
// that stores the raw header) must copy it with strings.Clone.
type headerCarrier struct {
	ctx *celeris.Context
}

func (h headerCarrier) Get(key string) string {
	v := h.ctx.Header(key)
	if key == "traceparent" {
		return v
	}
	return strings.Clone(v)
}

func (h headerCarrier) Set(key, value string) {
	h.ctx.SetHeader(key, value)
}

// keysLinearDedupe is the most distinct names Keys de-duplicates by
// comparing each with those already kept (cheaper than a map for a request's
// usual dozen headers); above it Keys uses a map.
const keysLinearDedupe = 32

// Keys returns the distinct request header names, without HTTP/2
// pseudo-headers, as copies built from one allocation (the slice is the
// second). The names are substrings of that one string, so a propagator that
// keeps any one of them keeps all of them alive: the sum of the request's
// header-name bytes, which the header limits bound.
func (h headerCarrier) Keys() []string {
	headers := h.ctx.RequestHeaders()
	keys := make([]string, 0, len(headers))
	var seen map[string]struct{}
	size := 0
	for _, hdr := range headers {
		name := hdr[0]
		// Skip HTTP/2 pseudo-headers (e.g. :method, :path).
		if len(name) > 0 && name[0] == ':' {
			continue
		}
		if seen == nil {
			if slices.Contains(keys, name) {
				continue
			}
			if len(keys) == keysLinearDedupe {
				seen = make(map[string]struct{}, len(headers))
				for _, k := range keys {
					seen[k] = struct{}{}
				}
			}
		}
		if seen != nil {
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
		}
		keys = append(keys, name)
		size += len(name)
	}
	// keys holds views of the request now: replace them with substrings of
	// one string made here.
	var b strings.Builder
	b.Grow(size)
	for _, k := range keys {
		b.WriteString(k)
	}
	all := b.String()
	off := 0
	for i, k := range keys {
		keys[i] = all[off : off+len(k)]
		off += len(k)
	}
	return keys
}
