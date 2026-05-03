package hertz

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
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
// other perfmatrix competitors. Hertz has hertz-contrib/sessions with
// Redis backing, but the 256-byte JSON round-trip is byte-identical on
// the wire; the hand-roll keeps the dep graph trimmed.
type redisSessionStore struct{ rdb *goredis.Client }

// mountDriverHandlers attaches the 4 driver routes to the hertz engine.
// pgx/v5 + go-redis/v9 + gomemcache mirror celeris's drivers.
func mountDriverHandlers(s *Server, svcs *services.Handles) {
	if s == nil {
		return
	}
	s.buildDriverState(svcs)

	s.mu.Lock()
	s.ensureEngineLocked(nil)
	mounted := s.mountedDriver
	s.mountedDriver = true
	s.mu.Unlock()
	if mounted {
		return
	}
	state := s.drivers

	s.h.GET("/db/user/:id", func(c context.Context, rc *app.RequestContext) {
		pg := state.pgPool()
		if pg == nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("postgres unavailable"))
			return
		}
		id, perr := strconv.Atoi(rc.Param("id"))
		if perr != nil {
			rc.SetStatusCode(http.StatusBadRequest)
			_, _ = rc.Write([]byte("bad id"))
			return
		}
		ctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		var row userRow
		if err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("pg error"))
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write(mustJSON(row))
	})

	s.h.GET("/cache/:key", func(c context.Context, rc *app.RequestContext) {
		rdb := state.redisClient()
		if rdb == nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("redis unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, rc.Param("key")).Bytes()
		if err != nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("redis get"))
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write(val)
	})

	s.h.GET("/mc/:key", func(_ context.Context, rc *app.RequestContext) {
		mc := state.mcClient()
		if mc == nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("memcached unavailable"))
			return
		}
		item, err := mc.Get(rc.Param("key"))
		if err != nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("mc get"))
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write(item.Value)
	})

	s.h.POST("/session", func(c context.Context, rc *app.RequestContext) {
		sess := state.sessionStore()
		if sess == nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("session unavailable"))
			return
		}
		sid := string(rc.Cookie("pmsid"))
		ctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		data := make(map[string]any, 4)
		if sid != "" {
			raw, err := sess.rdb.Get(ctx, "pmsess:"+sid).Bytes()
			if err == nil {
				_ = json.Unmarshal(raw, &data)
			} else if err != goredis.Nil {
				rc.SetStatusCode(http.StatusServiceUnavailable)
				_, _ = rc.Write([]byte("session load"))
				return
			}
		}
		if sid == "" {
			sid = uuid.NewString()
		}
		body := rc.Request.Body()
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
		if err := sess.save(c, sid, data); err != nil {
			rc.SetStatusCode(http.StatusServiceUnavailable)
			_, _ = rc.Write([]byte("session save"))
			return
		}
		ck := &protocol.Cookie{}
		ck.SetKey("pmsid")
		ck.SetValue(sid)
		ck.SetPath("/")
		ck.SetHTTPOnly(true)
		rc.Response.Header.SetCookie(ck)
		rc.SetContentType("application/json")
		rc.SetStatusCode(http.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true, Seq: newSeq}))
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
