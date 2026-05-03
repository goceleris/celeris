package healthcheck

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/goceleris/celeris"
)

// Pre-serialized JSON responses avoid per-request encoding overhead.
var (
	jsonOK          = []byte(`{"status":"ok"}`)
	jsonUnavailable = []byte(`{"status":"unavailable"}`)
)

const jsonContentType = "application/json"

// doneChanPool reuses the result channel allocated on the goroutine
// check path — mirrors middleware/timeout's chanPool pattern.
var doneChanPool = sync.Pool{New: func() any { return make(chan bool, 1) }}

// New creates a healthcheck middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfigCopy()
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()
	// Detect whether each checker is still the built-in always-true
	// default (either because the caller left it nil or because it was
	// filled by applyDefaults). Those defaults are trivial, so the
	// goroutine/channel/context.WithTimeout scaffolding in runChecker is
	// pure overhead — force FastPathTimeout for them regardless of what
	// CheckerTimeout says. Non-default checkers keep the user-configured
	// timeout.
	liveDefault := isDefaultChecker(cfg.LiveChecker, defaultConfig.LiveChecker)
	readyDefault := isDefaultChecker(cfg.ReadyChecker, defaultConfig.ReadyChecker)
	startDefault := isDefaultChecker(cfg.StartChecker, defaultConfig.StartChecker)

	livePath := cfg.LivePath
	readyPath := cfg.ReadyPath
	startPath := cfg.StartPath

	liveChecker := cfg.LiveChecker
	readyChecker := cfg.ReadyChecker
	startChecker := cfg.StartChecker
	liveTimeout := cfg.CheckerTimeout
	readyTimeout := cfg.CheckerTimeout
	startTimeout := cfg.CheckerTimeout
	if liveDefault {
		liveTimeout = FastPathTimeout
	}
	if readyDefault {
		readyTimeout = FastPathTimeout
	}
	if startDefault {
		startTimeout = FastPathTimeout
	}

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		method := c.Method()
		if method != "GET" && method != "HEAD" {
			return c.Next()
		}

		path := c.Path()

		var ok bool
		switch {
		case livePath != "" && path == livePath:
			ok = runChecker(liveChecker, c, liveTimeout)
		case readyPath != "" && path == readyPath:
			ok = runChecker(readyChecker, c, readyTimeout)
		case startPath != "" && path == startPath:
			ok = runChecker(startChecker, c, startTimeout)
		default:
			return c.Next()
		}
		return respond(c, ok, method == "HEAD")
	}
}

// isDefaultChecker reports whether checker is (a literal function
// value equal to) def. Used to detect whether the caller accepted the
// built-in always-true default so New can force FastPathTimeout and
// skip the context.WithTimeout + goroutine + channel scaffolding that
// only matters for checkers that might actually block.
func isDefaultChecker(checker, def Checker) bool {
	if checker == nil || def == nil {
		return false
	}
	return reflect.ValueOf(checker).Pointer() == reflect.ValueOf(def).Pointer()
}

// runChecker runs the health checker. When timeout is positive, the
// checker runs in a goroutine with a context deadline. When timeout is
// zero or negative, the checker is called synchronously without
// goroutine/channel/context overhead (fast-path for trivial checkers).
//
// A panicking checker is treated as a failure (returns false) rather
// than crashing the server.
func runChecker(checker Checker, c *celeris.Context, timeout time.Duration) bool {
	if timeout <= 0 {
		return safeCheck(checker, c)
	}

	origCtx := c.Context()
	ctx, cancel := context.WithTimeout(origCtx, timeout)
	c.SetContext(ctx)

	done := doneChanPool.Get().(chan bool)
	// Drain any stale value left from a prior pool cycle.
	select {
	case <-done:
	default:
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- false
			}
		}()
		done <- checker(c)
	}()

	var result bool
	select {
	case result = <-done:
	case <-ctx.Done():
		// Timeout fired. Cancel the context, then wait for the
		// goroutine to finish so it no longer touches *Context
		// (which may be recycled by the caller).
		cancel()
		<-done
		cancel = func() {} // prevent double-cancel in defer
	}
	doneChanPool.Put(done)

	cancel()
	c.SetContext(origCtx)
	return result
}

func safeCheck(checker Checker, c *celeris.Context) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return checker(c)
}

func respond(c *celeris.Context, ok bool, head bool) error {
	status := 503
	body := jsonUnavailable
	if ok {
		status = 200
		body = jsonOK
	}
	if head {
		return c.NoContent(status)
	}
	return c.Blob(status, jsonContentType, body)
}
