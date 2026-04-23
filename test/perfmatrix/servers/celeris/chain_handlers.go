package celeris

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/timeout"
)

// discardSlog is a slog.Logger whose handler drops every record. The
// logger middleware still performs the formatting work, so the bench
// observes realistic overhead without writing to stderr/disk.
var discardSlog = slog.New(slog.NewTextHandler(io.Discard, nil))

func newDiscardSlog() *slog.Logger { return discardSlog }

// mountChainHandlers mounts the 4 middleware chains (api, auth, security,
// fullstack) under /chain/<name>/... on srv. Each chain terminates in the
// same 2 handlers so scenarios differ only by middleware stack depth,
// matching the benchmark matrix's 4 × 3 shape (3 workloads reuse the 2
// handlers — the 1-conn probe shares the GET handler).
//
// Chains use celeris's own middleware packages (middleware/requestid,
// /logger, /recovery, /cors, /basicauth, /csrf, /secure, /ratelimit,
// /timeout, /bodylimit) so benchmark numbers reflect the celeris hot path
// end-to-end.
func mountChainHandlers(srv *celeris.Server, lifetime context.Context) {
	// Shared terminal handlers: every chain mounts these under its own
	// prefix. They are cheap on purpose — the bench exists to measure
	// middleware overhead, not handler work.
	jsonHandler := func(c *celeris.Context) error {
		return c.JSON(200, payloadSmall{
			Message: "Hello, World!",
			Server:  "celeris",
		})
	}
	uploadHandler := func(c *celeris.Context) error {
		_ = c.Body()
		return c.StatusBlob("text/plain; charset=utf-8", []byte("OK"))
	}

	// api chain: requestid + logger + recovery + cors.
	api := srv.Group("/chain/api",
		requestid.New(),
		loggerToDiscard(),
		recovery.New(),
		cors.New(),
	)
	api.GET("/json", jsonHandler)
	api.POST("/upload", uploadHandler)

	// auth chain: api + basicauth.
	auth := srv.Group("/chain/auth",
		requestid.New(),
		loggerToDiscard(),
		recovery.New(),
		cors.New(),
		basicauth.New(basicauth.Config{
			Users: map[string]string{"bench": "bench"},
			// Keep realm predictable for clients; default is "Restricted".
			Realm: "perfmatrix",
		}),
	)
	auth.GET("/json", jsonHandler)
	auth.POST("/upload", uploadHandler)

	// security chain: auth + csrf + secure.
	// csrf on safe methods is a no-op apart from cookie issuance; on
	// POST it validates the double-submit cookie. The bench loadgen
	// does not carry CSRF tokens, so we configure csrf.Config.Skipper
	// to bypass validation — the middleware still runs (to measure its
	// overhead) but does not reject authenticated requests.
	security := srv.Group("/chain/security",
		requestid.New(),
		loggerToDiscard(),
		recovery.New(),
		cors.New(),
		basicauth.New(basicauth.Config{
			Users: map[string]string{"bench": "bench"},
			Realm: "perfmatrix",
		}),
		csrf.New(csrf.Config{
			Skip: func(*celeris.Context) bool { return true },
		}),
		secure.New(),
	)
	security.GET("/json", jsonHandler)
	security.POST("/upload", uploadHandler)

	// fullstack chain: security + ratelimit + timeout + bodylimit.
	// RPS set high enough that no bench rig will exceed it — we want to
	// measure the ratelimit overhead, not actually drop requests.
	fullstack := srv.Group("/chain/fullstack",
		requestid.New(),
		loggerToDiscard(),
		recovery.New(),
		cors.New(),
		basicauth.New(basicauth.Config{
			Users: map[string]string{"bench": "bench"},
			Realm: "perfmatrix",
		}),
		csrf.New(csrf.Config{
			Skip: func(*celeris.Context) bool { return true },
		}),
		secure.New(),
		ratelimit.New(ratelimit.Config{
			RPS:   1_000_000,
			Burst: 1_000_000,
			// Tie the eviction goroutine's lifetime to the server's;
			// otherwise each cell leaks one goroutine here and the
			// matrix reliably trips Go's 10000-thread limit.
			CleanupContext: lifetime,
		}),
		timeout.New(timeout.Config{
			Timeout: 30 * time.Second,
		}),
		bodylimit.New(bodylimit.Config{
			Limit: "10MB",
		}),
	)
	fullstack.GET("/json", jsonHandler)
	fullstack.POST("/upload", uploadHandler)
}

// loggerToDiscard returns the logger middleware configured with a sink
// that drops every record. Logger overhead is interesting; actual log
// IO would poison bench output.
func loggerToDiscard() celeris.HandlerFunc {
	return logger.New(logger.Config{
		// A nil Output defaults to stderr. Install the no-op slog
		// handler so we measure formatter/dispatch cost without
		// disk/stderr IO.
		Output: newDiscardSlog(),
	})
}
