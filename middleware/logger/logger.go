package logger

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris"
)

var attrPool = sync.Pool{New: func() any {
	s := make([]slog.Attr, 0, 32)
	return &s
}}

// cachedPID is resolved once at init; the process ID never changes.
var cachedPID = os.Getpid()

// New creates a logger middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfigCopy()
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	// Resolve sensitive headers: nil → defaults, empty slice → disabled.
	headers := cfg.SensitiveHeaders
	if headers == nil {
		headers = DefaultSensitiveHeaders()
	}
	sensitiveMap := make(map[string]struct{}, len(headers))
	for _, h := range headers {
		sensitiveMap[strings.ToLower(h)] = struct{}{}
	}

	handler := cfg.Output.Handler()
	if cfg.DisableColors {
		disableHandlerColors(handler)
	}
	levelFn := cfg.Level
	fieldsFn := cfg.Fields
	doneFn := cfg.Done
	captureReqBody := cfg.CaptureRequestBody
	captureRespBody := cfg.CaptureResponseBody
	maxCapture := cfg.MaxCaptureBytes
	logHost := cfg.LogHost
	logUserAgent := cfg.LogUserAgent
	logReferer := cfg.LogReferer
	logRoute := cfg.LogRoute
	logPID := cfg.LogPID
	logQuery := cfg.LogQueryParams
	logForm := cfg.LogFormValues
	logCookies := cfg.LogCookies
	logBytesIn := cfg.LogBytesIn
	logScheme := cfg.LogScheme
	logContextKeys := cfg.LogContextKeys
	logRespHeaders := cfg.LogResponseHeaders
	// Build lowercased set for O(1) response header matching.
	respHeaderSet := make(map[string]struct{}, len(logRespHeaders))
	for _, h := range logRespHeaders {
		respHeaderSet[strings.ToLower(h)] = struct{}{}
	}
	// Build lowercased set for sensitive form field matching.
	var sensitiveFormSet map[string]struct{}
	if cfg.SensitiveFormFields != nil {
		sensitiveFormSet = make(map[string]struct{}, len(cfg.SensitiveFormFields))
		for _, f := range cfg.SensitiveFormFields {
			sensitiveFormSet[strings.ToLower(f)] = struct{}{}
		}
	}
	tz := cfg.TimeZone

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		path := c.Path()
		if _, ok := skipMap[path]; ok {
			return c.Next()
		}

		if captureRespBody {
			c.CaptureResponse()
		}

		err := c.Next()
		latency := time.Since(c.StartTime())

		status := c.StatusCode()
		level := levelFn(status)

		ctx := c.Context()
		if !handler.Enabled(ctx, level) {
			if doneFn != nil {
				doneFn(c, latency, status)
			}
			return err
		}

		attrsPtr := attrPool.Get().(*[]slog.Attr)
		attrs := (*attrsPtr)[:0]

		attrs = append(attrs,
			slog.String("method", c.Method()),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Duration("latency", latency),
			slog.Int("bytes", c.BytesWritten()),
		)

		if logPID {
			attrs = append(attrs, slog.Int("pid", cachedPID))
		}
		if ip := c.ClientIP(); ip != "" {
			attrs = append(attrs, slog.String("client_ip", ip))
		}
		if rid, ok := c.Get(celeris.RequestIDKey); ok {
			if s, ok := rid.(string); ok && s != "" {
				attrs = append(attrs, slog.String("request_id", s))
			}
		} else if rid := c.Header("x-request-id"); rid != "" {
			attrs = append(attrs, slog.String("request_id", rid))
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		if logHost {
			if h := c.Host(); h != "" {
				attrs = append(attrs, slog.String("host", h))
			}
		}
		if logUserAgent {
			if ua := c.Header("user-agent"); ua != "" {
				attrs = append(attrs, slog.String("user_agent", ua))
			}
		}
		if logReferer {
			if ref := c.Header("referer"); ref != "" {
				attrs = append(attrs, slog.String("referer", ref))
			}
		}
		if logRoute {
			if fp := c.FullPath(); fp != "" {
				attrs = append(attrs, slog.String("route", fp))
			}
		}
		if logQuery {
			if q := c.RawQuery(); q != "" {
				attrs = append(attrs, slog.String("query", q))
			}
		}
		if logForm {
			ct := c.Header("content-type")
			if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				if vals, parseErr := url.ParseQuery(string(c.Body())); parseErr == nil && len(vals) > 0 {
					if sensitiveFormSet != nil {
						for k := range vals {
							if _, ok := sensitiveFormSet[strings.ToLower(k)]; ok {
								vals.Set(k, "[REDACTED]")
							}
						}
					}
					attrs = append(attrs, slog.String("form", vals.Encode()))
				}
			}
		}
		if logCookies {
			if raw := c.Header("cookie"); raw != "" {
				names := parseCookieNames(raw)
				if names != "" {
					attrs = append(attrs, slog.String("cookies", names))
				}
			}
		}
		if logBytesIn {
			if cl := c.ContentLength(); cl >= 0 {
				attrs = append(attrs, slog.Int64("bytes_in", cl))
			}
		}
		if logScheme {
			if s := c.Scheme(); s != "" {
				attrs = append(attrs, slog.String("scheme", s))
			}
		}
		for _, key := range logContextKeys {
			if val, ok := c.Get(key); ok {
				switch v := val.(type) {
				case string:
					attrs = append(attrs, slog.String("ctx."+key, v))
				default:
					attrs = append(attrs, slog.String("ctx."+key, fmt.Sprint(v)))
				}
			}
		}
		if len(respHeaderSet) > 0 {
			for _, kv := range c.ResponseHeaders() {
				key := strings.ToLower(kv[0])
				if _, ok := respHeaderSet[key]; ok {
					attrs = append(attrs, slog.String("resp_header."+key, kv[1]))
				}
			}
		}
		if fieldsFn != nil {
			attrs = append(attrs, fieldsFn(c, latency)...)
		}
		if captureReqBody {
			body := c.Body()
			if len(body) > maxCapture {
				body = body[:maxCapture]
			}
			attrs = append(attrs, slog.String("request_body", string(body)))
		}
		if captureRespBody {
			body := c.ResponseBody()
			if len(body) > maxCapture {
				body = body[:maxCapture]
			}
			attrs = append(attrs, slog.String("response_body", string(body)))
		}
		// Sensitive header redaction: always emit every configured header
		// with "[REDACTED]" so the log entry is identical whether the
		// header was sent or not (constant-presence, no info leak).
		if len(sensitiveMap) > 0 {
			for h := range sensitiveMap {
				attrs = append(attrs, slog.String("header."+h, "[REDACTED]"))
			}
		}

		ts := c.StartTime()
		if tz != nil {
			ts = ts.In(tz)
		}

		r := slog.NewRecord(ts, level, "request", 0)
		r.AddAttrs(attrs...)
		_ = handler.Handle(ctx, r)

		// Cap the pooled slice to prevent unbounded growth from requests
		// with many custom fields. If it grew past 64, replace with a
		// fresh allocation so the oversized backing array is GC'd.
		if cap(attrs) > 64 {
			fresh := make([]slog.Attr, 0, 32)
			*attrsPtr = fresh
		} else {
			*attrsPtr = attrs
		}
		attrPool.Put(attrsPtr)

		if doneFn != nil {
			doneFn(c, latency, status)
		}

		return err
	}
}

// disableHandlerColors sets color=false on FastHandler or groupHandler.
func disableHandlerColors(h slog.Handler) {
	switch v := h.(type) {
	case *FastHandler:
		v.color = false
	case *groupHandler:
		v.color = false
	}
}

// parseCookieNames extracts cookie names from a raw Cookie header value,
// returning them as a comma-separated string. Values are intentionally
// omitted for security.
func parseCookieNames(raw string) string {
	var b strings.Builder
	first := true
	for len(raw) > 0 {
		// Skip leading whitespace and semicolons.
		i := 0
		for i < len(raw) && (raw[i] == ' ' || raw[i] == ';') {
			i++
		}
		raw = raw[i:]
		if raw == "" {
			break
		}
		// Find end of this cookie pair.
		end := strings.IndexByte(raw, ';')
		var pair string
		if end < 0 {
			pair = raw
			raw = ""
		} else {
			pair = raw[:end]
			raw = raw[end+1:]
		}
		// Extract name (before '=').
		eq := strings.IndexByte(pair, '=')
		var name string
		if eq < 0 {
			name = strings.TrimSpace(pair)
		} else {
			name = strings.TrimSpace(pair[:eq])
		}
		if name == "" {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(name)
		first = false
	}
	return b.String()
}
