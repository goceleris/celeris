package healthcheck

import (
	"context"
	"time"

	"github.com/goceleris/celeris"
)

// Pre-serialized JSON responses avoid per-request encoding overhead.
var (
	jsonOK          = []byte(`{"status":"ok"}`)
	jsonUnavailable = []byte(`{"status":"unavailable"}`)
)

const jsonContentType = "application/json"

// New creates a healthcheck middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfigCopy()
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	livePath := cfg.LivePath
	readyPath := cfg.ReadyPath
	startPath := cfg.StartPath

	liveChecker := cfg.LiveChecker
	readyChecker := cfg.ReadyChecker
	startChecker := cfg.StartChecker
	checkerTimeout := cfg.CheckerTimeout

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
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
			ok = runChecker(liveChecker, c, checkerTimeout)
		case readyPath != "" && path == readyPath:
			ok = runChecker(readyChecker, c, checkerTimeout)
		case startPath != "" && path == startPath:
			ok = runChecker(startChecker, c, checkerTimeout)
		default:
			return c.Next()
		}
		return respond(c, ok, method == "HEAD")
	}
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

	done := make(chan bool, 1)
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
