package recovery

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"syscall"

	"github.com/goceleris/celeris"
)

var stackPool = sync.Pool{New: func() any {
	buf := make([]byte, 4096)
	return &buf
}}

// New creates a recovery middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)

	handler := cfg.ErrorHandler
	brokenPipeHandler := cfg.BrokenPipeHandler
	stackSize := cfg.StackSize
	logStack := !cfg.DisableLogStack
	stackAll := cfg.StackAll
	disableBrokenPipeLog := cfg.DisableBrokenPipeLog
	log := cfg.Logger
	logLevel := cfg.LogLevel

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	return func(c *celeris.Context) (retErr error) {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(r)
				}

				panicVal := formatPanic(r)

				if isBrokenPipe(r) {
					retErr = handleBrokenPipe(c, r, panicVal, log, brokenPipeHandler, disableBrokenPipeLog)
					return
				}

				logPanic(c, log, logLevel, panicVal, logStack, stackSize, stackAll)

				rid, _ := c.Get(celeris.RequestIDKey)
				ridStr, _ := rid.(string)

				if c.Context().Err() != nil {
					log.LogAttrs(c.Context(), slog.LevelWarn, "panic after context cancelled",
						slog.String("request_id", ridStr),
						slog.String("method", c.Method()),
						slog.String("path", c.Path()),
						slog.String("error", panicVal),
					)
					retErr = fmt.Errorf("recovery: panic: %v", r)
					return
				}

				if c.IsWritten() {
					log.LogAttrs(c.Context(), logLevel, "panic after response committed",
						slog.String("request_id", ridStr),
						slog.String("method", c.Method()),
						slog.String("path", c.Path()),
						slog.String("error", panicVal),
					)
					retErr = fmt.Errorf("recovery: panic after response committed: %v", r)
					return
				}

				retErr = safeCallHandler(c, handler, r, log, logLevel)
			}
		}()

		return c.Next()
	}
}

// formatPanic converts a recovered panic value to a string.
func formatPanic(r any) string {
	switch v := r.(type) {
	case string:
		return v
	case error:
		return v.Error()
	default:
		return fmt.Sprint(r)
	}
}

// handleBrokenPipe handles panics caused by broken pipe / connection reset.
func handleBrokenPipe(c *celeris.Context, r any, panicVal string, log *slog.Logger, brokenPipeHandler func(*celeris.Context, any) error, disableLog bool) error {
	if !disableLog {
		rid, _ := c.Get(celeris.RequestIDKey)
		ridStr, _ := rid.(string)
		log.LogAttrs(c.Context(), slog.LevelWarn, "broken pipe",
			slog.String("request_id", ridStr),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.String("error", panicVal),
		)
	}
	if brokenPipeHandler != nil {
		return brokenPipeHandler(c, r)
	}
	return fmt.Errorf("recovery: broken pipe (client disconnected): %v", r)
}

// logPanic logs the panic with an optional stack trace.
func logPanic(c *celeris.Context, log *slog.Logger, level slog.Level, panicVal string, logStack bool, stackSize int, stackAll bool) {
	if !logStack {
		return
	}
	rid, _ := c.Get(celeris.RequestIDKey)
	ridStr, _ := rid.(string)
	if stackSize > 0 {
		bufPtr := stackPool.Get().(*[]byte)
		buf := *bufPtr
		if cap(buf) > 2*stackSize {
			buf = make([]byte, stackSize)
		} else if len(buf) < stackSize {
			buf = make([]byte, stackSize)
		}
		n := runtime.Stack(buf[:stackSize], stackAll)
		log.LogAttrs(c.Context(), level, "panic recovered",
			slog.String("request_id", ridStr),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.String("error", panicVal),
			slog.String("stack", string(buf[:n])),
		)
		*bufPtr = buf
		stackPool.Put(bufPtr)
	} else {
		log.LogAttrs(c.Context(), level, "panic recovered",
			slog.String("request_id", ridStr),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.String("error", panicVal),
		)
	}
}

// safeCallHandler calls the error handler, recovering from panics within it.
func safeCallHandler(c *celeris.Context, handler func(*celeris.Context, any) error, r any, log *slog.Logger, level slog.Level) (retErr error) {
	defer func() {
		if r2 := recover(); r2 != nil {
			rid, _ := c.Get(celeris.RequestIDKey)
			ridStr, _ := rid.(string)
			log.LogAttrs(c.Context(), level, "panic in error handler",
				slog.String("request_id", ridStr),
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.String("error", fmt.Sprint(r2)),
			)
			retErr = defaultErrorHandler(c, r2)
		}
	}()
	return handler(c, r)
}

func isBrokenPipe(v any) bool {
	err, ok := v.(error)
	if !ok {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, syscall.EPIPE) || errors.Is(opErr.Err, syscall.ECONNRESET) || errors.Is(opErr.Err, syscall.ECONNABORTED)
	}
	return false
}
