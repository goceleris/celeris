package stdhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// newSessionID returns an RFC 4122 UUIDv4 as the session identifier. Not
// cryptographically better than crypto/rand directly but matches what
// every competitor session library uses in practice.
func newSessionID() string { return uuid.NewString() }

// driverState is the per-Server driver handle bundle. Always attached to
// the Server in buildDriverState so route closures can re-read the
// pointers after a Stop + Start cycle without being re-registered.
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

// redisSessionStore is a 20-line Redis-backed session. Cookies name pmsid
// and carry an opaque identifier. Each POST merges the body JSON into the
// stored blob and increments seq. This avoids dragging in
// github.com/rbcervilla/redisstore (and its transitive gorilla/context
// chain) for a bench whose only goal is a Redis GET+SET round-trip.
//
// Godoc note on the choice: we hand-rolled rather than pulling in a full
// gorilla/sessions store because the 256-byte blob round-trip is the
// same on the wire either way, and the hand-roll saves 10+ transitive
// modules across 5 net/http-based framework packages.
type redisSessionStore struct {
	rdb *goredis.Client
}

// mountDriverHandlers attaches the 4 driver scenarios onto s. The routes
// are registered exactly once per Server; driver clients are (re)built
// on every call so Start(svcs) can point the same routes at a fresh
// service handle between runs.
//
// Uses pgxpool (PG), go-redis/v9 (Redis), gomemcache (memcached) as the
// stdlib-friendly equivalents of celeris's in-tree drivers.
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

	s.Mount(http.MethodGet, "/db/user/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pg := state.pgPool()
		if pg == nil {
			http.Error(w, "postgres unavailable", http.StatusServiceUnavailable)
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/db/user/")
		if idStr == "" || strings.Contains(idStr, "/") {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
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
	}))

	s.Mount(http.MethodGet, "/cache/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rdb := state.redisClient()
		if rdb == nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/cache/")
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			http.Error(w, "redis get", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(val)
	}))

	s.Mount(http.MethodGet, "/mc/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mc := state.mcClient()
		if mc == nil {
			http.Error(w, "memcached unavailable", http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/mc/")
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		item, err := mc.Get(key)
		if err != nil {
			http.Error(w, "mc get", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(item.Value)
	}))

	s.Mount(http.MethodPost, "/session", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
}

// buildDriverState constructs the driver clients from svcs and stores
// them on s.drivers. Idempotent across repeat Start calls: the old
// clients are closed before the new ones are installed.
func (s *Server) buildDriverState(svcs *services.Handles) {
	if s.drivers == nil {
		s.drivers = &driverState{}
	}
	ds := s.drivers
	ds.mu.Lock()
	defer ds.mu.Unlock()

	// Close any previously opened clients so repeat Start cycles don't
	// leak pool connections.
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

// load retrieves or creates a session blob from Redis. Returns the
// session id, the decoded map (empty for new sessions), and an error.
func (s *redisSessionStore) load(r *http.Request) (string, map[string]any, error) {
	sid := ""
	if ck, err := r.Cookie("pmsid"); err == nil {
		sid = ck.Value
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := make(map[string]any, 4)
	if sid != "" {
		raw, err := s.rdb.Get(ctx, sessionKey(sid)).Bytes()
		if err == nil {
			_ = json.Unmarshal(raw, &data)
		} else if err != goredis.Nil {
			return "", nil, err
		}
	}
	if sid == "" {
		sid = newSessionID()
	}
	return sid, data, nil
}

// save persists the session blob under sid in Redis with a 10-minute TTL.
func (s *redisSessionStore) save(ctx context.Context, sid string, data map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	buf, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, sessionKey(sid), buf, 10*time.Minute).Err()
}

func sessionKey(sid string) string { return "pmsess:" + sid }

// shutdownDriverHandlers closes the driver clients opened by
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

// userRow mirrors services.Seed's users table row.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// mustJSON serialises v, panicking on failure (the struct types we pass
// here never fail marshal).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
