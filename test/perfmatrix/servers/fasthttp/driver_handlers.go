package fasthttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/valyala/fasthttp"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// driverState bundles the driver clients used by the 4 driver scenarios.
// Stored on *Server so Stop can close them on teardown and repeat Start
// cycles swap in fresh clients without leaking pool connections.
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
// other perfmatrix competitors. Fasthttp has github.com/fasthttp/session
// with a Redis provider, but the 256-byte JSON round-trip this scenario
// measures is byte-identical; the hand-roll keeps the dep graph tight.
type redisSessionStore struct{ rdb *goredis.Client }

// mountDriverHandlers attaches the 4 driver routes as native fasthttp
// handlers on the Server's extras map.
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

	// Path matching: the fasthttp Server's dispatch does exact matches
	// for the static endpoints. For driver routes with dynamic path
	// parameters we install a prefix handler inside dispatch via the
	// extras map. Since extras keys are "METHOD path", registering
	// "GET /db/user/" doesn't match "/db/user/42" — we instead register
	// a handler for the exact path the extras map looks up. To handle
	// the param, wrap the extras lookup with a prefix-matcher.
	s.MountNative(http.MethodGet, "/db/user/", func(rc *fasthttp.RequestCtx) {
		pg := state.pgPool()
		if pg == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("postgres unavailable")
			return
		}
		path := string(rc.Path())
		idStr := strings.TrimPrefix(path, "/db/user/")
		if idStr == "" || strings.Contains(idStr, "/") {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			_, _ = rc.WriteString("bad id")
			return
		}
		id, perr := strconv.Atoi(idStr)
		if perr != nil {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			_, _ = rc.WriteString("bad id")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var row userRow
		if err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("pg error")
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(row))
	})

	s.MountNative(http.MethodGet, "/cache/", func(rc *fasthttp.RequestCtx) {
		rdb := state.redisClient()
		if rdb == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("redis unavailable")
			return
		}
		path := string(rc.Path())
		key := strings.TrimPrefix(path, "/cache/")
		if key == "" {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			_, _ = rc.WriteString("missing key")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("redis get")
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(val)
	})

	s.MountNative(http.MethodGet, "/mc/", func(rc *fasthttp.RequestCtx) {
		mc := state.mcClient()
		if mc == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("memcached unavailable")
			return
		}
		path := string(rc.Path())
		key := strings.TrimPrefix(path, "/mc/")
		if key == "" {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			_, _ = rc.WriteString("missing key")
			return
		}
		item, err := mc.Get(key)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("mc get")
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(item.Value)
	})

	s.MountNative(http.MethodPost, "/session", func(rc *fasthttp.RequestCtx) {
		sess := state.sessionStore()
		if sess == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("session unavailable")
			return
		}
		sid := string(rc.Request.Header.Cookie("pmsid"))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data := make(map[string]any, 4)
		if sid != "" {
			raw, err := sess.rdb.Get(ctx, "pmsess:"+sid).Bytes()
			if err == nil {
				_ = json.Unmarshal(raw, &data)
			} else if err != goredis.Nil {
				rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
				_, _ = rc.WriteString("session load")
				return
			}
		}
		if sid == "" {
			sid = uuid.NewString()
		}
		body := rc.PostBody()
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
		if err := sess.save(context.Background(), sid, data); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			_, _ = rc.WriteString("session save")
			return
		}
		ck := fasthttp.AcquireCookie()
		ck.SetKey("pmsid")
		ck.SetValue(sid)
		ck.SetPath("/")
		ck.SetHTTPOnly(true)
		rc.Response.Header.SetCookie(ck)
		fasthttp.ReleaseCookie(ck)
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
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
