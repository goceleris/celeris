//go:build linux

package integration_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/internal/engine"
)

// A driver that reaches a server's event-loop provider right after Start has
// begun, before Listen has built the loops and before every loop has created
// its epoll descriptor, must get a provider it can use: the engine is published
// by prepare, ahead of Listen, so EventLoopProvider() used to hand out an
// engine whose WorkerLoop panicked ("WorkerLoop called before Listen") and,
// once the loops existed, whose epoll loops still held epollFD -1 while a
// driver's epoll_ctl read it unsynchronised (a data race under -race, and
// "bad file descriptor" from RegisterConn). Run with -race.
//
// CELERIS_START_REGISTER_ITERS overrides the iteration count of every engine.

func startRegisterIters(def int) int {
	if v, err := strconv.Atoi(os.Getenv("CELERIS_START_REGISTER_ITERS")); err == nil && v > 0 {
		return v
	}
	return def
}

// startRaceFakes holds the fake backends shared by the iterations of one test.
type startRaceFakes struct {
	pgDSN, redisAddr, mcAddr string
}

func startMemcachedFake(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake memcached listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "version") {
						_, _ = c.Write([]byte("VERSION 1.6.0\r\n"))
					} else {
						_, _ = c.Write([]byte("ERROR\r\n"))
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func startRegisterFakes(t *testing.T) startRaceFakes {
	t.Helper()
	pg := startFakePG(t, pgSelect1Handler)
	host, port, _ := net.SplitHostPort(pg.Addr())
	rd := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			respSimple(w, "OK")
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "PING":
			respSimple(w, "PONG")
		case "SUBSCRIBE":
			_, _ = w.WriteString("*3\r\n")
			respBulk(w, "subscribe")
			respBulk(w, cmd[1])
			respInt(w, 1)
		default:
			respSimple(w, "OK")
		}
	})
	return startRaceFakes{
		pgDSN:     fmt.Sprintf("postgres://u@%s:%s/d?sslmode=disable", host, port),
		redisAddr: rd.Addr(),
		mcAddr:    startMemcachedFake(t),
	}
}

// guard runs f, turning a panic into a recorded failure so one bad iteration
// does not take the test binary down and the count of failures is visible.
func guard(what string, errs chan<- string, f func() error) {
	defer func() {
		if r := recover(); r != nil {
			errs <- fmt.Sprintf("%s: PANIC: %v", what, r)
		}
	}()
	if err := f(); err != nil {
		errs <- fmt.Sprintf("%s: %v", what, err)
	}
}

// oneStartRegisterIteration starts a server on eng and, with no wait for it to
// be ready, opens every driver the server's provider reaches. async selects
// the server: with an .Async() route the drivers take their direct path (the
// Redis Pub/Sub and PostgreSQL Connector connections still register with the
// engine), without one they take the integrated path that asks the provider
// for a WorkerLoop at construction. It returns the failures, and skip=true if
// the engine cannot start on this host.
func oneStartRegisterIteration(f startRaceFakes, eng celeris.EngineType, async bool) (failures []string, skip string) {
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: eng})
	if async {
		srv.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") }).Async()
	} else {
		srv.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") })
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return []string{"listen: " + err.Error()}, ""
	}
	stopped := make(chan struct{})
	var startErr error
	go func() {
		startErr = srv.StartWithListener(ln)
		close(stopped)
	}()

	errs := make(chan string, 64)
	var wg sync.WaitGroup
	run := func(what string, fn func() error) {
		wg.Go(func() { guard(what, errs, fn) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The provider itself, the moment it is visible: it must be usable.
	run("provider", func() error {
		var prov engine.EventLoopProvider
		giveUp := time.Now().Add(30 * time.Second)
		for prov == nil && time.Now().Before(giveUp) {
			select {
			case <-stopped: // the engine did not start: nothing to register on
				return nil
			default:
			}
			prov = srv.EventLoopProvider()
			runtime.Gosched()
		}
		if prov == nil {
			return nil // the engine never started (checked below), or the server hides it until ready
		}
		if prov.NumWorkers() < 1 {
			return fmt.Errorf("EventLoopProvider visible with NumWorkers() = %d", prov.NumWorkers())
		}
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) }()
		wl := prov.WorkerLoop(0)
		if err := wl.RegisterConn(fds[0], func([]byte) {}, func(error) {}); err != nil {
			return fmt.Errorf("RegisterConn: %w", err)
		}
		return wl.UnregisterConn(fds[0])
	})
	// database/sql through a Connector bound to the engine: registers the connection with the loop.
	run("postgres Connector", func() error {
		c, err := postgres.NewConnector(f.pgDSN)
		if err != nil {
			return err
		}
		db := sql.OpenDB(c.WithEngine(srv))
		defer func() { _ = db.Close() }()
		var n int
		return db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	})
	// The native Pool: direct on an async server, integrated (provider.WorkerLoop) on a sync one.
	run("postgres Pool", func() error {
		p, err := postgres.Open(f.pgDSN, postgres.WithEngine(srv), postgres.WithMaxOpen(2))
		if err != nil {
			return err
		}
		defer func() { _ = p.Close() }()
		var n int
		return p.QueryRow(ctx, "SELECT 1").Scan(&n)
	})
	// Redis: the command pool (direct, or integrated on a sync server) and a Pub/Sub connection (always on the loop).
	run("redis", func() error {
		c, err := redis.NewClient(f.redisAddr, redis.WithEngine(srv), redis.WithForceRESP2())
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		if err := c.Ping(ctx); err != nil {
			return fmt.Errorf("Ping: %w", err)
		}
		ps, err := c.Subscribe(ctx, "start-register")
		if err != nil {
			return fmt.Errorf("Subscribe: %w", err)
		}
		return ps.Close()
	})
	// Memcached: the integrated client asks the provider for a WorkerLoop in NewClient.
	run("memcached", func() error {
		c, err := memcached.NewClient(f.mcAddr, memcached.WithEngine(srv))
		if err != nil {
			return err
		}
		return c.Close()
	})

	wg.Wait()
	close(errs)
	for e := range errs {
		failures = append(failures, e)
	}

	// An engine that could not start on this host (io_uring without the kernel support) is a skip, not a failure.
	select {
	case <-stopped:
		if startErr != nil && strings.Contains(startErr.Error(), "not available") {
			return nil, startErr.Error()
		}
		if startErr != nil {
			failures = append(failures, "Start returned: "+startErr.Error())
		}
		return failures, ""
	default:
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		failures = append(failures, "Start did not return after Shutdown")
	}
	return failures, ""
}

func TestDriversOpenedRightAfterStart(t *testing.T) {
	f := startRegisterFakes(t)
	for _, tc := range []struct {
		name  string
		eng   celeris.EngineType
		iters int
	}{
		{"epoll", celeris.Epoll, 60},
		{"adaptive", celeris.Adaptive, 40},
		{"iouring", celeris.IOUring, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iters := startRegisterIters(tc.iters)
			counts := map[string]int{}
			for i := 0; i < iters; i++ {
				async := i%2 == 0
				fails, skip := oneStartRegisterIteration(f, tc.eng, async)
				if skip != "" {
					t.Skip(skip)
				}
				for _, m := range fails {
					counts[m]++
				}
			}
			for m, n := range counts {
				t.Errorf("%d of %d iterations: %s", n, iters, m)
			}
		})
	}
}
