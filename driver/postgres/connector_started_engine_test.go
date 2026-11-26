//go:build linux && postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	pg "github.com/goceleris/celeris/driver/postgres"
)

// The database/sql Connector opened WithEngine, called from a handler that runs
// on the engine's own worker. Every worker of a started epoll, io_uring or
// adaptive server runs its handlers inline; when each of those handlers asks the
// Connector for a new connection, the worker that must read the server's startup
// reply is the one waiting for it, and the server stops answering. The Connector
// dials without the event loop in that case (the way a Pool does; see
// loopLacksSyncPath).
//
// Build tags linux and postgres: the Drivers workflow runs this (it needs a
// server, CELERIS_PG_DSN; skipped without it).
//
// Each cell serves a fixed number of requests from a fresh server with a
// watchdog: a stalled cell fails, dumps the goroutines when CELERIS_STACKS_DIR
// names a directory, and is abandoned (its server and DB are not closed: both
// would wait on the stuck workers), so one run names every failing cell.

const (
	cseWatchdog   = 12 * time.Second
	cseHTTPClient = 8 * time.Second
)

// A cell is one server and one way of reaching PostgreSQL from its handler.
//
//	kind     "connector": a Connector WithEngine(srv); "none": a Connector with no engine;
//	         "pool": the native Pool WithEngine(srv), the control
//	started  the server runs when the DB is built and first used (otherwise both happen before Start)
//	handler  "inline": runs on the engine worker; "async-config": Config.AsyncHandlers (a route that only
//	         inherits it runs inline until a run of it blocks); "async-route": the route is marked Async
//	         (a goroutine of its own)
//	nested   the handler opens a second connection while it reads a result of 1000 rows
type cseCell struct {
	kind       string
	engine     celeris.EngineType
	engineName string
	adaptStart string // CELERIS_ADAPTIVE_START for adaptive cells
	started    bool
	handler    string
	conc       int
	nested     bool
}

func (c cseCell) name() string {
	st := "unstarted"
	if c.started {
		st = "started"
	}
	shape := "one"
	if c.nested {
		shape = "nested"
	}
	engineName := c.engineName
	switch c.kind {
	case "none":
		engineName = "connector-noengine"
	case "pool":
		engineName = "pool-" + c.engineName
	}
	return fmt.Sprintf("%s/%s/%s/conc%d/%s", engineName, st, c.handler, c.conc, shape)
}

// expectedStall marks the cells whose connections stay on the driver's event
// loop (a Connector with no engine, one built before Start, or on std) and whose
// handler reads a result while it opens a second connection: the paused-consumer
// stall of the loop path, a documented limit this change does not touch
// (celeris#1436). The stall is not deterministic (it needs several goroutines
// under CPU load), so the cells are skipped unless CELERIS_CSE_EXPECTED_STALL=1,
// and then only log the outcome.
func (c cseCell) expectedStall() bool {
	return c.nested && c.kind != "pool" && (c.kind == "none" || !c.started || c.engine == celeris.Std)
}

func cseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CELERIS_PG_DSN")
	if dsn == "" {
		t.Skip("skipping: CELERIS_PG_DSN not set")
	}
	return dsn
}

// cseStart starts srv in the background and waits until it listens (and, for a
// native engine, until its event loops are visible to drivers). A start error
// that names io_uring skips the test: the kernel or the container lacks it.
func cseStart(t *testing.T, srv *celeris.Server, wantProvider bool) string {
	t.Helper()
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start() }()
	for i := 0; i < 500; i++ {
		select {
		case err := <-startErr:
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "io_uring") {
				t.Skipf("io_uring is not available here (%v); the cell needs it", err)
			}
			t.Fatalf("server start: %v", err)
		default:
		}
		if a := srv.Addr(); a != nil && (!wantProvider || srv.EventLoopProvider() != nil) {
			return a.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the server never listened")
	return ""
}

func TestConnectorStartedEngine(t *testing.T) {
	dsn := cseDSN(t)
	engines := []cseCell{
		{engine: celeris.Epoll, engineName: "epoll"},
		{engine: celeris.IOUring, engineName: "iouring"},
		{engine: celeris.Adaptive, engineName: "adaptive", adaptStart: "epoll"},
		{engine: celeris.Adaptive, engineName: "adaptive-iouring-start", adaptStart: "iouring"},
		{engine: celeris.Std, engineName: "std"},
	}
	shapes := []struct {
		conc   int
		nested bool
	}{{1, false}, {16, false}, {1, true}}
	var cells []cseCell
	add := func(base cseCell, started []bool, handlers []string) {
		for _, st := range started {
			for _, h := range handlers {
				for _, sh := range shapes {
					c := base
					c.started, c.handler, c.conc, c.nested = st, h, sh.conc, sh.nested
					cells = append(cells, c)
				}
			}
		}
	}
	for _, e := range engines {
		e.kind = "connector"
		add(e, []bool{false, true}, []string{"inline", "async-config", "async-route"})
	}
	// A Connector with no engine on a server that is running or not: always the driver's own loop.
	add(cseCell{kind: "none", engine: celeris.Epoll, engineName: "epoll"}, []bool{false, true}, []string{"inline", "async-route"})
	// The native Pool, the control: it takes the same rule, and a started server was always fine for it.
	for _, e := range engines[:3] {
		e.kind = "pool"
		add(e, []bool{true}, []string{"inline", "async-route"})
	}
	for _, c := range cells {
		if testing.Short() && (c.nested || c.engine == celeris.IOUring || c.adaptStart == "iouring") {
			continue // short mode: epoll, adaptive and std, plain queries
		}
		t.Run(c.name(), func(t *testing.T) { runCSECell(t, dsn, c) })
	}
}

// cseQuery runs the handler's statements: one SELECT, or, nested, a result of
// 1000 rows with a second query run while the first is being read.
type cseQuery func(ctx context.Context, nested bool) error

func cseSQLQuery(db *sql.DB) cseQuery {
	return func(ctx context.Context, nested bool) error {
		if !nested {
			var x int
			return db.QueryRowContext(ctx, "SELECT 1").Scan(&x)
		}
		rows, err := db.QueryContext(ctx, "SELECT g FROM generate_series(1,1000) g")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			var g int
			if err := rows.Scan(&g); err != nil {
				return err
			}
			if n == 0 {
				var x int
				if err := db.QueryRowContext(ctx, "SELECT 2").Scan(&x); err != nil {
					return err
				}
			}
			n++
		}
		return rows.Err()
	}
}

func csePoolQuery(p *pg.Pool) cseQuery {
	return func(ctx context.Context, nested bool) error {
		if !nested {
			var x int
			return p.QueryRow(ctx, "SELECT 1").Scan(&x)
		}
		rows, err := p.QueryContext(ctx, "SELECT g FROM generate_series(1,1000) g")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			var g int
			if err := rows.Scan(&g); err != nil {
				return err
			}
			if n == 0 {
				var x int
				if err := p.QueryRow(ctx, "SELECT 2").Scan(&x); err != nil {
					return err
				}
			}
			n++
		}
		return rows.Err()
	}
}

func runCSECell(t *testing.T, dsn string, c cseCell) {
	if c.expectedStall() && os.Getenv("CELERIS_CSE_EXPECTED_STALL") != "1" {
		t.Skip("EXPECTED-STALL: the event-loop path can stall when a handler opens a second connection while it " +
			"reads a result (celeris#1436); the stall needs 4 or more goroutines under CPU load and is not " +
			"deterministic (it was seen at 10 of 100 cold runs with 4 goroutines), so these cells stay skipped; " +
			"set CELERIS_CSE_EXPECTED_STALL=1 to run them")
	}
	if c.adaptStart != "" {
		t.Setenv("CELERIS_ADAPTIVE_START", c.adaptStart)
	}
	srv := celeris.New(celeris.Config{Engine: c.engine, Addr: "127.0.0.1:0", Workers: 2, AsyncHandlers: c.handler == "async-config"})

	// The handler reads the query through a pointer set once the DB is built, which for
	// a started server is after the handlers can run: the race detector sees no
	// ordering through the network.
	var qp atomic.Pointer[cseQuery]
	var closeDB func()
	build := func() {
		var q cseQuery
		switch c.kind {
		case "pool":
			p, err := pg.Open(dsn, pg.WithEngine(srv), pg.WithMaxOpen(32))
			if err != nil {
				t.Fatal(err)
			}
			q, closeDB = csePoolQuery(p), func() { _ = p.Close() }
		default:
			cn, err := pg.NewConnector(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if c.kind == "connector" {
				cn = cn.WithEngine(srv)
			}
			db := sql.OpenDB(cn)
			db.SetMaxOpenConns(32)
			q, closeDB = cseSQLQuery(db), func() { _ = db.Close() }
		}
		qp.Store(&q)
	}
	route := srv.GET("/q", func(ctx *celeris.Context) error {
		if err := (*qp.Load())(ctx.Context(), c.nested); err != nil {
			return ctx.String(500, "err: %v", err)
		}
		return ctx.String(200, "ok")
	})
	if c.handler == "async-route" {
		route.Async()
	}
	var addr string
	if c.started {
		addr = cseStart(t, srv, c.engine != celeris.Std)
		build()
	} else {
		build()
		// A connection dialled before Start stays on the driver's own loop.
		pctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := (*qp.Load())(pctx, false)
		cancel()
		if err != nil {
			t.Fatalf("query before Start: %v", err)
		}
		addr = cseStart(t, srv, false)
	}

	total := 120
	if c.conc == 1 {
		total = 60
	}
	if c.nested {
		total = 30
	}
	var done, bad atomic.Int64
	var firstErr atomic.Value
	var next atomic.Int64
	finished := make(chan struct{})
	client := &http.Client{Timeout: cseHTTPClient, Transport: &http.Transport{MaxIdleConnsPerHost: c.conc, MaxConnsPerHost: c.conc}}
	var wg sync.WaitGroup
	for i := 0; i < c.conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(total) {
				resp, err := client.Get("http://" + addr + "/q")
				if err != nil {
					bad.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					bad.Add(1)
					firstErr.CompareAndSwap(nil, strconv.Itoa(resp.StatusCode)+" "+string(b))
					continue
				}
				done.Add(1)
			}
		}()
	}
	go func() { wg.Wait(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(cseWatchdog):
		buf := make([]byte, 16<<20)
		buf = buf[:runtime.Stack(buf, true)]
		dump := ""
		if dir := os.Getenv("CELERIS_STACKS_DIR"); dir != "" {
			dump = filepath.Join(dir, "cse-"+strings.ReplaceAll(c.name(), "/", "_")+".txt")
			_ = os.WriteFile(dump, buf, 0o644)
		}
		msg := fmt.Sprintf("STALL: %d of %d requests completed, %d failed within %v (first error %v); stacks %q",
			done.Load(), total, bad.Load(), cseWatchdog, firstErr.Load(), dump)
		if c.expectedStall() {
			t.Log("EXPECTED-STALL observed: " + msg)
			return
		}
		// Abandoned on purpose: closing the server or the DB would wait on the stuck workers.
		t.Error(msg)
		return
	}
	if c.expectedStall() {
		t.Logf("EXPECTED-STALL cell did not stall: %d requests completed, %d failed", done.Load(), bad.Load())
	}
	if bad.Load() != 0 || done.Load() != int64(total) {
		t.Errorf("%d of %d requests completed, %d failed (first error %v)", done.Load(), total, bad.Load(), firstErr.Load())
	}
	client.CloseIdleConnections()
	closeDB()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

// TestConnectorDialledBeforeStart: connections the Connector opened before Start
// stay on the driver's own event loop, and keep serving handlers that run inline
// on the started server's workers. The pool is capped at the connections opened
// before Start, so every handler query must come back from one of those
// backends (the same pg_backend_pid), proving they were reused.
func TestConnectorDialledBeforeStart(t *testing.T) {
	dsn := cseDSN(t)
	for _, e := range []struct {
		name string
		et   celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"adaptive", celeris.Adaptive}, {"iouring", celeris.IOUring}} {
		t.Run(e.name, func(t *testing.T) {
			if testing.Short() && e.et == celeris.IOUring {
				t.Skip("short mode")
			}
			srv := celeris.New(celeris.Config{Engine: e.et, Addr: "127.0.0.1:0", Workers: 2})
			cn, err := pg.NewConnector(dsn)
			if err != nil {
				t.Fatal(err)
			}
			const pre = 4
			db := sql.OpenDB(cn.WithEngine(srv))
			db.SetMaxOpenConns(pre)
			db.SetMaxIdleConns(pre)

			// Open `pre` connections at once, before Start.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conns := make([]*sql.Conn, pre)
			prePIDs := map[int]bool{}
			for i := range conns {
				c, err := db.Conn(ctx)
				if err != nil {
					t.Fatalf("connection %d before Start: %v", i, err)
				}
				conns[i] = c
				var pid int
				if err := c.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					t.Fatal(err)
				}
				prePIDs[pid] = true
			}
			for _, c := range conns {
				_ = c.Close()
			}
			if len(prePIDs) != pre {
				t.Fatalf("expected %d distinct backends before Start, got %d", pre, len(prePIDs))
			}

			var mu sync.Mutex
			seen := map[int]bool{}
			srv.GET("/q", func(c *celeris.Context) error {
				var pid int
				if err := db.QueryRowContext(c.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return c.String(500, "err: %v", err)
				}
				mu.Lock()
				seen[pid] = true
				mu.Unlock()
				return c.String(200, "%d", pid)
			})
			addr := cseStart(t, srv, e.et != celeris.Std)

			const total = 200
			var done, bad atomic.Int64
			var next atomic.Int64
			finished := make(chan struct{})
			client := &http.Client{Timeout: cseHTTPClient, Transport: &http.Transport{MaxIdleConnsPerHost: 16, MaxConnsPerHost: 16}}
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for next.Add(1) <= total {
						resp, err := client.Get("http://" + addr + "/q")
						if err != nil {
							bad.Add(1)
							continue
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode != 200 {
							bad.Add(1)
							continue
						}
						done.Add(1)
					}
				}()
			}
			go func() { wg.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(cseWatchdog):
				t.Errorf("STALL: %d of %d requests completed, %d failed within %v", done.Load(), total, bad.Load(), cseWatchdog)
				return
			}
			if done.Load() != total || bad.Load() != 0 {
				t.Errorf("%d of %d requests completed, %d failed", done.Load(), total, bad.Load())
			}
			mu.Lock()
			for pid := range seen {
				if !prePIDs[pid] {
					t.Errorf("a handler query ran on backend %d, which is not one of the connections opened before Start", pid)
				}
			}
			mu.Unlock()
			client.CloseIdleConnections()
			_ = db.Close()
			sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer scancel()
			_ = srv.Shutdown(sctx)
		})
	}
}

// TestConnectorResultCapPerMode: a result of about 70 MB. A Connector bound to a
// started epoll server dials direct and reads the result whole, so it stops at
// the 64 MiB cap with ErrResultTooBig (from sql.Rows.Err); one bound before
// Start, and one with no engine, stay on the driver's loop and stream all of it
// (the no-engine case is what keeps a plain sql.Open uncapped).
func TestConnectorResultCapPerMode(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: reads a 70 MB result twice")
	}
	dsn := cseDSN(t)
	const nrows = 70000 // 70000 rows of 1000 bytes
	const q = "SELECT repeat('x', 1000) FROM generate_series(1, 70000)"
	read := func(db *sql.DB) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return n, err
			}
			n++
		}
		return n, rows.Err()
	}
	for _, mode := range []string{"no-engine", "unstarted", "started"} {
		t.Run(mode, func(t *testing.T) {
			srv := celeris.New(celeris.Config{Engine: celeris.Epoll, Addr: "127.0.0.1:0", Workers: 2})
			srv.GET("/", func(c *celeris.Context) error { return c.String(200, "ok") })
			cn, err := pg.NewConnector(dsn)
			if err != nil {
				t.Fatal(err)
			}
			var db *sql.DB
			if mode == "unstarted" {
				db = sql.OpenDB(cn.WithEngine(srv))
			}
			cseStart(t, srv, true)
			switch mode {
			case "started":
				db = sql.OpenDB(cn.WithEngine(srv))
			case "no-engine":
				db = sql.OpenDB(cn)
			}
			defer func() {
				_ = db.Close()
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			n, err := read(db)
			if mode != "started" && err != nil {
				// The loop's sync fast path can, rarely, read past the cap in one poll under load (see
				// ErrResultTooBig); a direct connection caps every time. A loop connection streams
				// everything in at least one of a few attempts.
				for i := 0; i < 3 && err != nil; i++ {
					n, err = read(db)
				}
			}
			if mode == "started" {
				// Direct stays strict: it caps on every attempt, no retry.
				for i := 0; i < 3; i++ {
					if i > 0 {
						n, err = read(db)
					}
					if !errors.Is(err, pg.ErrResultTooBig) {
						t.Fatalf("started, attempt %d: got %d rows and error %v, want ErrResultTooBig", i+1, n, err)
					}
					if n == 0 || n >= nrows {
						t.Fatalf("started, attempt %d: %d rows before the cap, want some but not all of %d", i+1, n, nrows)
					}
				}
			} else if err != nil || n != nrows {
				t.Fatalf("%s: %d rows, error %v; want all %d and no error (the loop streams)", mode, n, err, nrows)
			}
		})
	}
}

// A Connector bound to a server that has since shut down asks the engine's
// WorkerLoop for the dial mode before it dials: it must neither panic nor hang.
// (The engines keep their loops after Shutdown, so the connection is dialled
// direct and works; the result is only logged.)
func TestConnectorAfterServerShutdown(t *testing.T) {
	dsn := cseDSN(t)
	for _, e := range []struct {
		name string
		et   celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"adaptive", celeris.Adaptive}} {
		et := e.et
		t.Run(e.name, func(t *testing.T) {
			srv := celeris.New(celeris.Config{Engine: et, Addr: "127.0.0.1:0", Workers: 2})
			srv.GET("/", func(c *celeris.Context) error { return c.String(200, "ok") })
			cseStart(t, srv, true)
			cn, err := pg.NewConnector(dsn)
			if err != nil {
				t.Fatal(err)
			}
			db := sql.OpenDB(cn.WithEngine(srv))
			db.SetMaxIdleConns(0) // every call dials
			defer func() { _ = db.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				t.Fatalf("ping on the running server: %v", err)
			}
			sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer scancel()
			if err := srv.Shutdown(sctx); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			// The result (an error or a working direct connection) does not matter; reaching it does.
			err = db.PingContext(ctx)
			t.Logf("ping after shutdown: %v", err)
		})
	}
}
