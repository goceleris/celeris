package fiber

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	fiberv3 "github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// redisSessionStore: hand-rolled Redis-backed session matching the
// net/http-based competitors. Fiber ships its own
// middleware/session package with Redis storage (via
// gofiber/storage/redis/v3) but the 256-byte JSON round-trip is
// identical on the wire either way; the hand-roll keeps the dep graph
// aligned with every other competitor package.
type redisSessionStore struct{ rdb *goredis.Client }

// mountDriverHandlers attaches the 4 driver routes as native fiber
// handlers. pgx/v5 + go-redis/v9 + gomemcache match celeris's driver
// stack for a fair overhead comparison.
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

	s.app.Get("/db/user/:id", func(c fiberv3.Ctx) error {
		pg := state.pgPool()
		if pg == nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("postgres unavailable")
		}
		id, perr := strconv.Atoi(c.Params("id"))
		if perr != nil {
			c.Status(http.StatusBadRequest)
			return c.SendString("bad id")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		var row userRow
		if err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("pg error")
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(row))
	})

	s.app.Get("/cache/:key", func(c fiberv3.Ctx) error {
		rdb := state.redisClient()
		if rdb == nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("redis unavailable")
		}
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, c.Params("key")).Bytes()
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("redis get")
		}
		c.Set("Content-Type", "application/octet-stream")
		return c.Send(val)
	})

	s.app.Get("/mc/:key", func(c fiberv3.Ctx) error {
		mc := state.mcClient()
		if mc == nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("memcached unavailable")
		}
		item, err := mc.Get(c.Params("key"))
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("mc get")
		}
		c.Set("Content-Type", "application/octet-stream")
		return c.Send(item.Value)
	})

	s.app.Post("/session", func(c fiberv3.Ctx) error {
		sess := state.sessionStore()
		if sess == nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("session unavailable")
		}
		sid := string(c.Request().Header.Cookie("pmsid"))
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		data := make(map[string]any, 4)
		if sid != "" {
			raw, err := sess.rdb.Get(ctx, "pmsess:"+sid).Bytes()
			if err == nil {
				_ = json.Unmarshal(raw, &data)
			} else if err != goredis.Nil {
				c.Status(http.StatusServiceUnavailable)
				return c.SendString("session load")
			}
		}
		if sid == "" {
			sid = uuid.NewString()
		}
		body := c.Body()
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
		if err := sess.save(c.Context(), sid, data); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return c.SendString("session save")
		}
		c.Cookie(&fiberv3.Cookie{Name: "pmsid", Value: sid, Path: "/", HTTPOnly: true})
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true, Seq: newSeq}))
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

// Keep io import referenced in case downstream files split these types
// across compilation units.
var _ = io.EOF
