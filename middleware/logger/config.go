package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/goceleris/celeris"
)

// defaultSensitiveHeaders is the immutable source list. Callers get a
// copy via DefaultSensitiveHeaders() to prevent mutation.
var defaultSensitiveHeadersList = [...]string{
	"authorization",
	"cookie",
	"set-cookie",
	"x-api-key",
}

// DefaultSensitiveHeaders returns a copy of the default sensitive header
// names redacted when [Config].SensitiveHeaders is nil. Set SensitiveHeaders
// to an empty slice ([]string{}) to disable all redaction.
func DefaultSensitiveHeaders() []string {
	out := make([]string, len(defaultSensitiveHeadersList))
	copy(out, defaultSensitiveHeadersList[:])
	return out
}

// defaultSensitiveFormFieldsList is the immutable source list for form fields.
var defaultSensitiveFormFieldsList = [...]string{
	"password",
	"passwd",
	"secret",
	"token",
	"access_token",
	"refresh_token",
	"credit_card",
	"card_number",
	"cvv",
	"ssn",
}

// DefaultSensitiveFormFields returns a copy of the default sensitive form
// field names. Unlike [DefaultSensitiveHeaders], this list is NOT applied
// automatically — [Config].SensitiveFormFields nil means no redaction.
// Pass this list explicitly when enabling [Config].LogFormValues to get
// reasonable defaults:
//
//	logger.New(logger.Config{
//	    LogFormValues:       true,
//	    SensitiveFormFields: logger.DefaultSensitiveFormFields(),
//	})
func DefaultSensitiveFormFields() []string {
	out := make([]string, len(defaultSensitiveFormFieldsList))
	copy(out, defaultSensitiveFormFieldsList[:])
	return out
}

// Config defines the logger middleware configuration.
type Config struct {
	// Skip defines a function to conditionally bypass this middleware.
	// When it returns true for a given request, no log entry is emitted
	// and c.Next() is called directly.
	Skip func(c *celeris.Context) bool

	// Output is the slog.Logger used to emit log records.
	// When nil, slog.Default() is used.
	Output *slog.Logger

	// Level maps an HTTP response status code to a slog.Level, controlling
	// the severity of each log entry. The default maps 5xx to Error,
	// 4xx to Warn, and everything else to Info.
	Level func(status int) slog.Level

	// Fields is an optional callback that returns additional slog.Attr
	// values to include in each log entry. It is called after the
	// downstream handler completes, so latency and response data are
	// available.
	Fields func(c *celeris.Context, latency time.Duration) []slog.Attr

	// Done is an optional callback invoked after the log entry is written.
	// It is always called, even when the log level is disabled, making it
	// suitable for alerting on 5xx responses or collecting metrics outside
	// the log pipeline.
	Done func(c *celeris.Context, latency time.Duration, status int)

	// SkipPaths is a list of request paths to exclude from logging.
	// Matching is exact (no glob or prefix support).
	SkipPaths []string

	// CaptureRequestBody enables logging of the request body, truncated
	// to MaxCaptureBytes. The body is emitted as the "request_body" attr.
	CaptureRequestBody bool

	// CaptureResponseBody enables logging of the response body, truncated
	// to MaxCaptureBytes. The body is emitted as the "response_body" attr
	// and requires the response to be captured via c.CaptureResponse().
	CaptureResponseBody bool

	// MaxCaptureBytes is the maximum number of body bytes to include in
	// the log entry when CaptureRequestBody or CaptureResponseBody is
	// enabled. Default: 4096. This cap prevents OOM on large payloads.
	MaxCaptureBytes int

	// SensitiveHeaders lists header names whose values should be redacted
	// in log output. Matched values are replaced with "[REDACTED]" and
	// header names are compared case-insensitively.
	//
	// When nil, DefaultSensitiveHeaders is used automatically. Set to an
	// empty slice ([]string{}) to disable all header redaction.
	SensitiveHeaders []string

	// TimeZone sets the timezone applied to log timestamps before they
	// are passed to the slog handler. When nil, the local timezone is
	// used.
	TimeZone *time.Location

	// TimeFormat sets a custom Go time layout string (e.g. time.RFC3339)
	// for FastHandler output. When empty, FastHandler uses its built-in
	// zero-alloc RFC3339-millis formatter.
	TimeFormat string

	// LogHost includes the request Host header as the "host" slog attr.
	// Disabled by default to keep log entries compact.
	LogHost bool

	// LogUserAgent includes the User-Agent request header as the
	// "user_agent" slog attr. Disabled by default.
	LogUserAgent bool

	// LogReferer includes the Referer request header as the "referer"
	// slog attr. Disabled by default.
	LogReferer bool

	// LogRoute includes the matched route pattern from c.FullPath() as
	// the "route" slog attr. Omitted when the route is empty (e.g., 404).
	LogRoute bool

	// LogPID includes the process ID (cached at init) as the "pid" slog
	// attr. Useful for distinguishing workers in multi-process deployments.
	LogPID bool

	// LogQueryParams includes the raw query string from the request URL
	// as the "query" slog attr. Omitted when there is no query string.
	LogQueryParams bool

	// LogFormValues includes URL-encoded form field names and values as
	// the "form" slog attr. Only active when the request content-type is
	// application/x-www-form-urlencoded. Use SensitiveFormFields to
	// redact secrets.
	LogFormValues bool

	// LogCookies includes cookie names (not values, for security) as the
	// "cookies" slog attr. Values are intentionally omitted to avoid
	// leaking session tokens into logs.
	LogCookies bool

	// LogBytesIn includes the request Content-Length as the "bytes_in"
	// slog attr (int64). Omitted when the content length is negative or
	// absent.
	LogBytesIn bool

	// LogScheme includes the request scheme from c.Scheme() (e.g.,
	// "https") as the "scheme" slog attr.
	LogScheme bool

	// DisableColors overrides the Color flag on FastHandlerOptions. When
	// true, the FastHandler emits plain text without ANSI escape codes.
	// Useful for log files, CI pipelines, or any non-terminal output.
	DisableColors bool

	// LogResponseHeaders lists specific response header names whose values
	// should be included in the log entry. Names are compared
	// case-insensitively and each matched header is logged as
	// "resp_header.<lowercased-name>".
	LogResponseHeaders []string

	// SensitiveFormFields lists form field names whose values should be
	// redacted when LogFormValues is true. Matched values are replaced
	// with "[REDACTED]" and field names are compared case-insensitively.
	//
	// IMPORTANT: Unlike SensitiveHeaders (where nil uses defaults), nil
	// here means NO redaction at all — every form value is logged verbatim,
	// including passwords and secrets. This asymmetry is intentional
	// because form field names are application-specific.
	//
	// Use [DefaultSensitiveFormFields] for a reasonable starting list:
	//
	//   SensitiveFormFields: logger.DefaultSensitiveFormFields()
	//
	// Semantics:
	//   - nil  → no form field redaction (all values logged as-is).
	//   - []string{} (empty) → no form field redaction (same as nil).
	//   - non-empty → matching fields are replaced with "[REDACTED]".
	SensitiveFormFields []string

	// LogContextKeys lists context store keys whose values should be
	// included in the log entry. For each key, the middleware calls
	// c.Get(key) and, if found, emits the value as a "ctx.<key>" slog
	// attr using fmt.Sprint for non-string types.
	LogContextKeys []string
}

// defaultConfigCopy returns the default logger configuration.
func defaultConfigCopy() Config { return Config{} }

func applyDefaults(cfg Config) Config {
	if cfg.Output == nil {
		cfg.Output = slog.Default()
	}
	if cfg.Level == nil {
		cfg.Level = defaultLevel
	}
	if cfg.MaxCaptureBytes <= 0 && (cfg.CaptureRequestBody || cfg.CaptureResponseBody) {
		cfg.MaxCaptureBytes = 4096
	}
	return cfg
}

func defaultLevel(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func (cfg Config) validate() {
	if cfg.MaxCaptureBytes < 0 && (cfg.CaptureRequestBody || cfg.CaptureResponseBody) {
		panic("logger: MaxCaptureBytes must not be negative when body capture is enabled")
	}
}

// CLFConfig returns a Config pre-configured for Common Log Format (CLF) style
// output. It enables LogHost, LogUserAgent, and LogReferer, and uses a Fields
// callback that emits a single "clf" attribute with the traditional
// combined log format line: host ident user [time] "method path proto" status bytes "referer" "user-agent".
func CLFConfig() Config {
	return Config{
		LogHost:      true,
		LogUserAgent: true,
		LogReferer:   true,
		Fields: func(c *celeris.Context, latency time.Duration) []slog.Attr {
			host := c.Host()
			if host == "" {
				host = "-"
			}
			ref := c.Header("referer")
			if ref == "" {
				ref = "-"
			}
			ua := c.Header("user-agent")
			if ua == "" {
				ua = "-"
			}
			proto := c.Header("x-forwarded-proto")
			if proto == "" {
				proto = "HTTP/1.1"
			} else {
				proto = strings.ToUpper(proto)
			}
			line := fmt.Sprintf(`%s - - [%s] "%s %s %s" %d %d "%s" "%s"`,
				host,
				time.Now().Add(-latency).UTC().Format("02/Jan/2006:15:04:05 -0700"),
				c.Method(), c.Path(), proto,
				c.StatusCode(), c.BytesWritten(),
				ref, ua,
			)
			return []slog.Attr{slog.String("clf", line)}
		},
	}
}

// JSONConfig returns a Config pre-configured for structured JSON output
// using slog.JSONHandler writing to os.Stdout. It enables LogHost,
// LogUserAgent, LogReferer, LogRoute, and LogQueryParams for
// comprehensive structured logging.
func JSONConfig() Config {
	return Config{
		Output:         slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		LogHost:        true,
		LogUserAgent:   true,
		LogReferer:     true,
		LogRoute:       true,
		LogQueryParams: true,
	}
}
