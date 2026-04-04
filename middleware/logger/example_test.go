package logger_test

import (
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/logger"
)

func ExampleNew() {
	// Zero-config: logs to slog.Default() with INFO/WARN/ERROR levels.
	// DefaultSensitiveHeaders are redacted automatically.
	_ = logger.New()
}

func ExampleNew_fastHandler() {
	// FastHandler: zero-allocation structured logging with color output.
	log := slog.New(logger.NewFastHandler(os.Stderr, &logger.FastHandlerOptions{
		Color: true,
	}))
	_ = logger.New(logger.Config{Output: log})
}

func ExampleNew_customFields() {
	// Add custom fields to every log entry.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	_ = logger.New(logger.Config{
		Output:    log,
		SkipPaths: []string{"/health", "/ready"},
		Fields: func(c *celeris.Context, latency time.Duration) []slog.Attr {
			return []slog.Attr{
				slog.String("trace_id", c.Header("x-trace-id")),
				slog.Bool("slow", latency > time.Second),
			}
		},
	})
}

func ExampleNew_sensitiveHeaders() {
	// Override the default sensitive headers list.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	_ = logger.New(logger.Config{
		Output: log,
		SensitiveHeaders: []string{
			"Authorization",
			"X-Api-Key",
			"X-Internal-Token",
		},
	})
}

func ExampleNew_disableRedaction() {
	// Pass an empty slice to disable all header redaction.
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	_ = logger.New(logger.Config{
		Output:           log,
		SensitiveHeaders: []string{},
	})
}
