package otel

import (
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
// Get and Keys return copies. On epoll and io_uring a request header is a
// view of the engine's receive buffer: safe to compare while the request is
// handled, unsafe to keep after it. Propagators keep what they read, in the
// context the middleware installs with SetContext (trace.ParseTraceState and
// baggage.Parse return substrings of their input; a propagator that walks
// Keys may keep the names it matches), and that context outlives the request
// whenever something keeps it: a detached WebSocket or SSE stream derives its
// context from it (celeris#714). A header that is absent costs nothing; one
// that is present costs one allocation of its size.
type headerCarrier struct {
	ctx *celeris.Context
}

func (h headerCarrier) Get(key string) string {
	return strings.Clone(h.ctx.Header(key))
}

func (h headerCarrier) Set(key, value string) {
	h.ctx.SetHeader(key, value)
}

func (h headerCarrier) Keys() []string {
	headers := h.ctx.RequestHeaders()
	seen := make(map[string]struct{}, len(headers))
	keys := make([]string, 0, len(headers))
	for _, hdr := range headers {
		// Skip HTTP/2 pseudo-headers (e.g. :method, :path).
		if len(hdr[0]) > 0 && hdr[0][0] == ':' {
			continue
		}
		if _, dup := seen[hdr[0]]; dup {
			continue
		}
		seen[hdr[0]] = struct{}{}
		keys = append(keys, strings.Clone(hdr[0]))
	}
	return keys
}
