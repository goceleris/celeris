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
	"github.com/goceleris/celeris/validation"
)

var (
	// ErrPanic is the base sentinel for all recovery-caught panics.
	ErrPanic = errors.New("recovery: panic")

	// ErrPanicContextCancelled is returned when a panic occurs after
	// the request context has been cancelled.
	ErrPanicContextCancelled = errors.New("recovery: panic (context cancelled)")

	// ErrPanicResponseCommitted is returned when a panic occurs after
	// the response has already been partially written.
	ErrPanicResponseCommitted = errors.New("recovery: panic (response committed)")

	// ErrBrokenPipe is returned when a panic is caused by a broken pipe
	// or connection reset (client disconnected).
	ErrBrokenPipe = errors.New("recovery: broken pipe")
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
	// ErrorHandlerErr takes precedence — wrap the panic value as an
	// error and route through the typed handler. Non-error panic values
	// (string / struct / etc.) are wrapped via fmt.Errorf so callers
	// can use errors.Is / errors.As.
	if cfg.ErrorHandlerErr != nil {
		typedFn := cfg.ErrorHandlerErr
		handler = func(c *celeris.Context, r any) error {
			if e, ok := r.(error); ok {
				return typedFn(c, e)
			}
			return typedFn(c, fmt.Errorf("recovery: panic: %v", r))
		}
	}
	brokenPipeHandler := cfg.BrokenPipeHandler
	stackSize := cfg.StackSize
	logStack := !cfg.DisableLogStack
	stackAll := cfg.StackAll
	disableBrokenPipeLog := cfg.DisableBrokenPipeLog
	log := cfg.Logger
	logLevel := cfg.LogLevel

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) (retErr error) {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(r)
				}

				// validation.PanicCount is a zero-cost no-op in
				// production builds; under -tags=validation it backs
				// probatorium's "no panic escaped recovery" predicate.
				validation.PanicCount.Add(1)

				panicVal := formatPanic(r)

				if isBrokenPipe(r) {
					retErr = handleBrokenPipe(c, r, panicVal, log, brokenPipeHandler, disableBrokenPipeLog)
					return
				}

				logPanic(c, log, logLevel, panicVal, logStack, stackSize, stackAll)

				ridStr := c.RequestID()

				if c.Context().Err() != nil {
					log.LogAttrs(c.Context(), slog.LevelWarn, "panic after context cancelled",
						slog.String("request_id", ridStr),
						slog.String("method", c.Method()),
						slog.String("path", c.Path()),
						slog.String("error", panicVal),
					)
					retErr = fmt.Errorf("%w: %v", ErrPanicContextCancelled, r)
					return
				}

				if c.IsWritten() {
					log.LogAttrs(c.Context(), logLevel, "panic after response committed",
						slog.String("request_id", ridStr),
						slog.String("method", c.Method()),
						slog.String("path", c.Path()),
						slog.String("error", panicVal),
					)
					retErr = fmt.Errorf("%w: %v", ErrPanicResponseCommitted, r)
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
		ridStr := c.RequestID()
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
	return fmt.Errorf("%w: %v", ErrBrokenPipe, r)
}

// logPanic logs the panic with an optional stack trace.
func logPanic(c *celeris.Context, log *slog.Logger, level slog.Level, panicVal string, logStack bool, stackSize int, stackAll bool) {
	if !logStack {
		return
	}
	ridStr := c.RequestID()
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
			ridStr := c.RequestID()
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
