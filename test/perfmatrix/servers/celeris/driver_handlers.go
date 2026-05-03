package celeris

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/session"
	"github.com/goceleris/celeris/middleware/session/redisstore"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// userRow mirrors services.Seed's users table row.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session; seq is the
// session's hit counter (incremented by every request on the same cookie).
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// mountDriverHandlers attaches the 4 driver scenarios onto srv. Drivers
// are constructed lazily from svcs; if svcs is nil or the relevant handle
// field is unset, the corresponding handler returns 503 so loadgen counts
// errors deterministically.
//
// The celeris variant uses the in-tree driver packages
// (driver/postgres, driver/redis, driver/memcached) and the celeris
// redis-backed session store. Pool size 16 per driver keeps the bench
// concurrency-bounded by the server, not the client.
//
// Driver clients are stored on the *celerisServer so Stop can close them
// and repeat Start cycles don't leak pool connections.
func mountDriverHandlers(s *celerisServer, srv *celeris.Server, svcs *services.Handles) {
	if s == nil || srv == nil {
		return
	}

	// Postgres: GET /db/user/:id
	if svcs != nil && svcs.Postgres != nil {
		pool, err := postgres.Open(svcs.Postgres.DSN, postgres.WithMaxOpen(16))
		if err == nil {
			s.pgPool = pool
		}
	}
	srv.GET("/db/user/:id", func(c *celeris.Context) error {
		if s.pgPool == nil {
			return c.AbortWithStatus(503)
		}
		idStr := c.Param("id")
		id, perr := strconv.Atoi(idStr)
		if perr != nil {
			return c.AbortWithStatus(400)
		}
		row := userRow{}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		qerr := s.pgPool.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
		if qerr != nil {
			return c.AbortWithStatus(503)
		}
		return c.JSON(200, row)
	})

	// Redis: GET /cache/:key
	if svcs != nil && svcs.Redis != nil {
		rdb, err := redis.NewClient(svcs.Redis.Addr, redis.WithPoolSize(16))
		if err == nil {
			s.redisClient = rdb
		}
	}
	srv.GET("/cache/:key", func(c *celeris.Context) error {
		if s.redisClient == nil {
			return c.AbortWithStatus(503)
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		val, err := s.redisClient.GetBytes(ctx, c.Param("key"))
		if err != nil {
			return c.AbortWithStatus(503)
		}
		return c.Blob(200, "application/octet-stream", val)
	})

	// Memcached: GET /mc/:key
	if svcs != nil && svcs.Memcached != nil {
		mc, err := memcached.NewClient(svcs.Memcached.Addr, memcached.WithMaxOpen(16))
		if err == nil {
			s.mcClient = mc
		}
	}
	srv.GET("/mc/:key", func(c *celeris.Context) error {
		if s.mcClient == nil {
			return c.AbortWithStatus(503)
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		val, err := s.mcClient.GetBytes(ctx, c.Param("key"))
		if err != nil {
			return c.AbortWithStatus(503)
		}
		return c.Blob(200, "application/octet-stream", val)
	})

	// Session: POST /session uses the celeris session middleware with a
	// Redis backend when Redis is available; otherwise the endpoint
	// responds 503.
	if svcs != nil && svcs.Redis != nil && s.redisClient != nil {
		store := redisstore.New(s.redisClient)
		s.sessionMW = session.New(session.Config{
			Store:       store,
			CookieName:  "pmsid",
			IdleTimeout: 10 * time.Minute,
		})
	}
	srv.POST("/session", func(c *celeris.Context) error {
		if s.sessionMW == nil {
			return c.AbortWithStatus(503)
		}
		// The session middleware was not registered globally on the
		// server (static handlers and other scenarios don't need it).
		// Run it inline here as a per-route wrapper so the session hit
		// counter only fires on /session requests.
		return s.sessionMW(c)
	}, sessionHandler)
}

// sessionHandler is the terminal handler for POST /session: it merges the
// request body into the session blob and responds with the current hit
// count. Wrapped by s.sessionMW in mountDriverHandlers so the middleware
// loads/saves the session transparently on every request.
func sessionHandler(c *celeris.Context) error {
	sess := session.FromContext(c)
	if sess == nil {
		return c.AbortWithStatus(503)
	}
	// Merge request body if it is JSON (the scenario POSTs a 256-byte
	// JSON-ish blob). Parse failures are non-fatal — the hit counter
	// still increments so the session round-trip is observable.
	body := c.Body()
	if len(body) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			for k, v := range payload {
				sess.Set(k, v)
			}
		}
	}
	seq := sess.GetInt("seq") + 1
	sess.Set("seq", seq)
	return c.JSON(200, sessionResponse{OK: true, Seq: seq})
}

// shutdownDriverHandlers closes any driver clients opened by
// mountDriverHandlers. Called from celerisServer.Stop so repeat
// Start/Stop cycles don't leak connections.
func shutdownDriverHandlers(s *celerisServer) {
	if s.pgPool != nil {
		_ = s.pgPool.Close()
		s.pgPool = nil
	}
	if s.redisClient != nil {
		_ = s.redisClient.Close()
		s.redisClient = nil
	}
	if s.mcClient != nil {
		_ = s.mcClient.Close()
		s.mcClient = nil
	}
	s.sessionMW = nil
}

// Compile-time guard so unused imports remain tied to real behavior.
var _ = errors.New
