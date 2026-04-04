package recovery

import (
	"log/slog"

	"github.com/goceleris/celeris"
)

// Config defines the recovery middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths is a list of request paths to exclude from recovery.
	// Matching is exact (no glob or prefix support).
	SkipPaths []string

	// ErrorHandler handles the recovered panic value. Default: JSON 500 response.
	ErrorHandler func(c *celeris.Context, err any) error

	// BrokenPipeHandler handles broken pipe / connection reset panics.
	// When nil, broken pipe panics are logged at WARN level without a stack
	// trace and the middleware returns a descriptive error.
	BrokenPipeHandler func(c *celeris.Context, err any) error

	// StackSize is the max bytes for stack trace capture. Default: 4096.
	// Set to 0 to disable stack capture (panic value, method, and path
	// are still logged). Negative values are silently replaced with the default.
	StackSize int

	// DisableLogStack disables all panic logging on recovery. When true,
	// panics are still caught and the error handler is called, but no log
	// entry is emitted. Default: false (panics are logged with stack traces).
	DisableLogStack bool

	// StackAll captures all goroutine stacks, not just the current one.
	// Default: false (current goroutine only).
	StackAll bool

	// DisableBrokenPipeLog suppresses the WARN-level log for broken pipe
	// and ECONNRESET panics. Default: false (log enabled).
	DisableBrokenPipeLog bool

	// Logger is the slog logger for panic logging. Default: slog.Default().
	Logger *slog.Logger

	// LogLevel is the slog level used for normal panic log entries.
	// Default: slog.LevelError. Broken pipe panics are always logged
	// at slog.LevelWarn regardless of this setting.
	LogLevel slog.Level
}

// defaultStackSize is the default stack trace capture size in bytes.
const defaultStackSize = 4096

// defaultConfig is the default recovery configuration.
var defaultConfig = Config{
	StackSize: defaultStackSize,
	LogLevel:  slog.LevelError,
}

func applyDefaults(cfg Config) Config {
	if cfg.ErrorHandler == nil {
		cfg.ErrorHandler = defaultErrorHandler
	}
	if cfg.StackSize < 0 {
		cfg.StackSize = defaultStackSize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg
}

var defaultErrorBody = []byte(`{"error":"Internal Server Error"}`)

func defaultErrorHandler(c *celeris.Context, _ any) error {
	return c.Blob(500, "application/json", defaultErrorBody)
}
