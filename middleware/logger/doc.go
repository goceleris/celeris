// Package logger provides HTTP request logging middleware for celeris.
//
// The middleware logs each request with configurable fields including
// method, path, status, latency, and bytes written. It supports
// both standard slog handlers and the zero-alloc [FastHandler].
//
// Basic usage with the default slog output:
//
//	server.Use(logger.New())
//
// Using the zero-alloc FastHandler with color output:
//
//	mw := logger.New(logger.Config{
//	    Output: slog.New(logger.NewFastHandler(os.Stderr, &logger.FastHandlerOptions{
//	        Color: true,
//	    })),
//	})
//	server.Use(mw)
//
// # Body Capture
//
// Set [Config].CaptureRequestBody and/or [Config].CaptureResponseBody to
// log request and response bodies. Bodies are truncated to
// [Config].MaxCaptureBytes (default 4096).
//
// # Sensitive Header Redaction
//
// [Config].SensitiveHeaders lists header names whose values are redacted.
// When nil, [DefaultSensitiveHeaders] is used. Set to an empty slice to
// disable all redaction.
//
// # Request ID Integration
//
// The middleware reads the request ID from the context store
// (key "request_id") first, falling back to the x-request-id header.
//
// # Predefined Configurations
//
// [CLFConfig] returns a Config for Common Log Format style output.
// [JSONConfig] returns a Config for structured JSON output.
// Both return a [Config] value that can be further customized:
//
//	cfg := logger.CLFConfig()
//	cfg.SkipPaths = []string{"/health"}
//	server.Use(logger.New(cfg))
//
// # Design Rationale
//
// All output is structured through Go's [log/slog] package. No template
// strings are provided. The [Config].Fields callback supplies arbitrary
// extensibility. [FastHandler] formats directly into pooled byte buffers,
// avoiding fmt.Sprintf and time.Format entirely.
//
// # Query Parameter Security
//
// Security: LogQueryParams logs raw query strings which may contain
// sensitive values (OAuth tokens, API keys, session identifiers).
// Consider using SkipPaths for sensitive endpoints or implementing a
// custom Fields function that redacts sensitive query parameters.
//
// # Skipping
//
// Set [Config].Skip to bypass dynamically, or [Config].SkipPaths for
// exact-match path exclusions.
//
// # Middleware Order
//
// Register after requestid for request ID inclusion in logs.
package logger
