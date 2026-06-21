// Package logger provides HTTP request logging middleware for celeris.
//
// [New] returns a [celeris.HandlerFunc] that logs each request with method,
// path, status, latency, bytes written, client IP, and request ID. All output
// is routed through Go's [log/slog] package; supply any slog.Handler via
// [Config].Output, or use [NewFastHandler] for zero-alloc pooled-buffer output.
//
// Two preset constructors cover the most common cases: [CLFConfig] produces
// Common Log Format style output; [JSONConfig] produces structured JSON via
// slog.JSONHandler. Both return a [Config] that can be further customised before
// passing to [New].
//
// Notable Config fields:
//   - CaptureRequestBody / CaptureResponseBody — log bodies, truncated to MaxCaptureBytes (default 4096).
//   - SensitiveHeaders — header values to redact; nil uses [DefaultSensitiveHeaders].
//   - LogFormValues / SensitiveFormFields — log form fields with optional redaction; use [DefaultSensitiveFormFields] as a safe starting list.
//   - Skip / SkipPaths — bypass the middleware dynamically or for exact-match paths.
//   - Fields / Done — callbacks for custom attributes and post-log hooks.
//   - LogContextKeys — emit arbitrary context-store values as "ctx.<key>" attributes.
//
// Register after the requestid middleware so request IDs are available in every
// log entry. When running behind a reverse proxy, install the proxy middleware
// via Server.Pre() before logger.New() so the real client IP is recorded.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability
package logger
