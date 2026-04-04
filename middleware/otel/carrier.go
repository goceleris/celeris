package otel

import (
	"github.com/goceleris/celeris"
	"go.opentelemetry.io/otel/propagation"
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
type headerCarrier struct {
	ctx *celeris.Context
}

func (h headerCarrier) Get(key string) string {
	return h.ctx.Header(key)
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
		keys = append(keys, hdr[0])
	}
	return keys
}
