package chi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	chiv5 "github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// driverState bundles the driver clients used by the 4 driver scenarios.
// Kept on *Server so repeat Start cycles close and rebuild cleanly.
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

// redisSessionStore is a 20-line Redis-backed session with a pmsid
// cookie. Hand-rolled rather than using github.com/rbcervilla/redisstore
// so the net/http competitor packages avoid an extra transitive module.
// The wire shape — 256-byte JSON GET/SET per request — is identical to
// what a gorilla/sessions Redis store would produce.
type redisSessionStore struct {
	rdb *goredis.Client
}

// mountDriverHandlers mounts the 4 driver routes on the chi router.
// Handlers respond 503 when the corresponding service is unavailable so
// loadgen sees a deterministic error count during services=none runs.
//
// pgx/v5 + go-redis/v9 + gomemcache mirror celeris's in-tree driver
// choices via the industry-standard bindings every other net/http-based
// competitor uses.
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

	s.router.Get("/db/user/{id}", func(w http.ResponseWriter, r *http.Request) {
		pg := state.pgPool()
		if pg == nil {
			http.Error(w, "postgres unavailable", http.StatusServiceUnavailable)
			return
		}
		idStr := chiv5.URLParam(r, "id")
		id, perr := strconv.Atoi(idStr)
		if perr != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var row userRow
		err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
		if err != nil {
			http.Error(w, "pg error", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, mustJSON(row))
	})

	s.router.Get("/cache/{key}", func(w http.ResponseWriter, r *http.Request) {
		rdb := state.redisClient()
		if rdb == nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, chiv5.URLParam(r, "key")).Bytes()
		if err != nil {
			http.Error(w, "redis get", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(val)
	})

	s.router.Get("/mc/{key}", func(w http.ResponseWriter, r *http.Request) {
		mc := state.mcClient()
		if mc == nil {
			http.Error(w, "memcached unavailable", http.StatusServiceUnavailable)
			return
		}
		item, err := mc.Get(chiv5.URLParam(r, "key"))
		if err != nil {
			http.Error(w, "mc get", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(item.Value)
	})

	s.router.Post("/session", func(w http.ResponseWriter, r *http.Request) {
		sess := state.sessionStore()
		if sess == nil {
			http.Error(w, "session unavailable", http.StatusServiceUnavailable)
			return
		}
		sid, data, err := sess.load(r)
		if err != nil {
			http.Error(w, "session load", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
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
			http.Error(w, "session save", http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "pmsid",
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
		})
		writeJSON(w, mustJSON(sessionResponse{OK: true, Seq: newSeq}))
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
			pool, perr := pgxpool.NewWithConfig(context.Background(), pgCfg)
			if perr == nil {
				ds.pg = pool
			}
		}
	}
	if svcs != nil && svcs.Redis != nil {
		ds.rdb = goredis.NewClient(&goredis.Options{
			Addr:     svcs.Redis.Addr,
			PoolSize: 16,
		})
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

// shutdownDriverHandlers closes any driver clients opened by
// mountDriverHandlers. Called from Stop.
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
