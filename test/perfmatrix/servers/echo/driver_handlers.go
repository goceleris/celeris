package echo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	echov4 "github.com/labstack/echo/v4"
	goredis "github.com/redis/go-redis/v9"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

type driverState struct {
	mu   sync.Mutex
	pg   *pgxpool.Pool
	rdb  *goredis.Client
	mc   *memcache.Client
	sess *redisSessionStore
}

func (d *driverState) pgPool() *pgxpool.Pool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pg
}
func (d *driverState) redisClient() *goredis.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rdb
}
func (d *driverState) mcClient() *memcache.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mc
}
func (d *driverState) sessionStore() *redisSessionStore {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sess
}

// redisSessionStore: 20-line Redis-backed session. Hand-rolled instead
// of wiring echo-contrib/session so the net/http competitor packages
// stay on the same shared wire-level implementation.
type redisSessionStore struct{ rdb *goredis.Client }

// mountDriverHandlers attaches the 4 driver routes. pgx/v5 + go-redis/v9
// + gomemcache mirror celeris's in-tree drivers for a fair comparison.
func mountDriverHandlers(s *Server, svcs *services.Handles) {
	if s == nil {
		return
	}
	s.buildDriverState(svcs)

	s.mu.Lock()
	mounted := s.mountedDriver
	s.mountedDriver = true
	s.mu.Unlock()
	if mounted {
		return
	}
	state := s.drivers

	s.e.GET("/db/user/:id", func(c echov4.Context) error {
		pg := state.pgPool()
		if pg == nil {
			return c.String(http.StatusServiceUnavailable, "postgres unavailable")
		}
		id, perr := strconv.Atoi(c.Param("id"))
		if perr != nil {
			return c.String(http.StatusBadRequest, "bad id")
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		var row userRow
		if err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			return c.String(http.StatusServiceUnavailable, "pg error")
		}
		return c.Blob(http.StatusOK, "application/json", mustJSON(row))
	})

	s.e.GET("/cache/:key", func(c echov4.Context) error {
		rdb := state.redisClient()
		if rdb == nil {
			return c.String(http.StatusServiceUnavailable, "redis unavailable")
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, c.Param("key")).Bytes()
		if err != nil {
			return c.String(http.StatusServiceUnavailable, "redis get")
		}
		return c.Blob(http.StatusOK, "application/octet-stream", val)
	})

	s.e.GET("/mc/:key", func(c echov4.Context) error {
		mc := state.mcClient()
		if mc == nil {
			return c.String(http.StatusServiceUnavailable, "memcached unavailable")
		}
		item, err := mc.Get(c.Param("key"))
		if err != nil {
			return c.String(http.StatusServiceUnavailable, "mc get")
		}
		return c.Blob(http.StatusOK, "application/octet-stream", item.Value)
	})

	s.e.POST("/session", func(c echov4.Context) error {
		sess := state.sessionStore()
		if sess == nil {
			return c.String(http.StatusServiceUnavailable, "session unavailable")
		}
		r := c.Request()
		sid, data, err := sess.load(r)
		if err != nil {
			return c.String(http.StatusServiceUnavailable, "session load")
		}
		body, _ := io.ReadAll(http.MaxBytesReader(c.Response(), r.Body, 1<<16))
		_ = r.Body.Close()
		if len(body) > 0 {
			var incoming map[string]any
			if jerr := json.Unmarshal(body, &incoming); jerr == nil {
				for k, v := range incoming {
					data[k] = v
				}
			}
		}
		seq, _ := data["seq"].(float64)
		newSeq := int(seq) + 1
		data["seq"] = newSeq
		if err := sess.save(r.Context(), sid, data); err != nil {
			return c.String(http.StatusServiceUnavailable, "session save")
		}
		c.SetCookie(&http.Cookie{Name: "pmsid", Value: sid, Path: "/", HttpOnly: true})
		return c.Blob(http.StatusOK, "application/json", mustJSON(sessionResponse{OK: true, Seq: newSeq}))
	})
}

func (s *Server) buildDriverState(svcs *services.Handles) {
	if s.drivers == nil {
		s.drivers = &driverState{}
	}
	ds := s.drivers
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.pg != nil {
		ds.pg.Close()
		ds.pg = nil
	}
	if ds.rdb != nil {
		_ = ds.rdb.Close()
		ds.rdb = nil
	}
	ds.mc = nil
	ds.sess = nil
	if svcs != nil && svcs.Postgres != nil {
		pgCfg, err := pgxpool.ParseConfig(svcs.Postgres.DSN)
		if err == nil {
			pgCfg.MaxConns = 16
			if pool, perr := pgxpool.NewWithConfig(context.Background(), pgCfg); perr == nil {
				ds.pg = pool
			}
		}
	}
	if svcs != nil && svcs.Redis != nil {
		ds.rdb = goredis.NewClient(&goredis.Options{Addr: svcs.Redis.Addr, PoolSize: 16})
		ds.sess = &redisSessionStore{rdb: ds.rdb}
	}
	if svcs != nil && svcs.Memcached != nil {
		ds.mc = memcache.New(svcs.Memcached.Addr)
		ds.mc.MaxIdleConns = 16
	}
}

func (s *redisSessionStore) load(r *http.Request) (string, map[string]any, error) {
	sid := ""
	if ck, err := r.Cookie("pmsid"); err == nil {
		sid = ck.Value
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := make(map[string]any, 4)
	if sid != "" {
		raw, err := s.rdb.Get(ctx, "pmsess:"+sid).Bytes()
		if err == nil {
			_ = json.Unmarshal(raw, &data)
		} else if err != goredis.Nil {
			return "", nil, err
		}
	}
	if sid == "" {
		sid = uuid.NewString()
	}
	return sid, data, nil
}

func (s *redisSessionStore) save(ctx context.Context, sid string, data map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	buf, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, "pmsess:"+sid, buf, 10*time.Minute).Err()
}

func (s *Server) shutdownDriverHandlers() {
	if s.drivers == nil {
		return
	}
	ds := s.drivers
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.pg != nil {
		ds.pg.Close()
		ds.pg = nil
	}
	if ds.rdb != nil {
		_ = ds.rdb.Close()
		ds.rdb = nil
	}
	ds.mc = nil
	ds.sess = nil
}

type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
