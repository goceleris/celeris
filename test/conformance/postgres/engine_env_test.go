//go:build postgres

package postgres_test

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	postgres "github.com/goceleris/celeris/driver/postgres"
)

// CELERIS_CONFORMANCE_ENGINE selects the engine the drivers under test are opened on:
//
//	unset / "none"    the drivers run standalone (no WithEngine): what these suites always did
//	"default"         WithEngine(srv), srv = celeris.New with Engine UNSET and a .Async() route, NOT started
//	                  (a pool opened before Start, the order of the HttpArena entry)
//	"default-started" the same server, started before the drivers open
//	"default-ready"   started, and an HTTP request to the .Async() route has been answered before the drivers open
//	"epoll", "epoll-started", "epoll-ready"  the same three with Engine: celeris.Epoll (the explicit-engine control)
//
// On Linux, "default" is the configuration a user gets by not setting Config.Engine.
var (
	engineSrvOnce sync.Once
	engineSrv     postgres.ServerProvider
)

func engineProvider() postgres.ServerProvider {
	mode := os.Getenv("CELERIS_CONFORMANCE_ENGINE")
	if mode == "" || mode == "none" {
		return nil
	}
	engineSrvOnce.Do(func() {
		cfg := celeris.Config{Addr: "127.0.0.1:0"}
		started, ready := false, false
		switch mode {
		case "default":
		case "default-started":
			started = true
		case "default-ready":
			started, ready = true, true
		case "epoll":
			cfg.Engine = celeris.Epoll
		case "epoll-started":
			cfg.Engine = celeris.Epoll
			started = true
		case "epoll-ready":
			cfg.Engine = celeris.Epoll
			started, ready = true, true
		default:
			panic("CELERIS_CONFORMANCE_ENGINE: unknown mode " + mode)
		}
		s := celeris.New(cfg)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") }).Async()
		if started {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				panic(err)
			}
			startErr := make(chan error, 1)
			go func() { startErr <- s.StartWithListener(ln) }()
			for i := 0; i < 100 && s.EventLoopProvider() == nil; i++ {
				select {
				case err := <-startErr:
					panic("CELERIS_CONFORMANCE_ENGINE=" + mode + ": the server did not start: " + fmt.Sprint(err))
				default:
				}
				time.Sleep(50 * time.Millisecond)
			}
			// On Linux a started native engine must expose its provider: without it the drivers silently run
			// standalone and the arm would pass without testing what it names.
			if runtime.GOOS == "linux" && s.EventLoopProvider() == nil {
				panic("CELERIS_CONFORMANCE_ENGINE=" + mode + ": the started server has no event-loop provider after 5 s")
			}
			if ready {
				for i := 0; i < 100; i++ {
					resp, err := http.Get("http://" + ln.Addr().String() + "/ping")
					if err == nil {
						_ = resp.Body.Close()
						if resp.StatusCode == 200 {
							break
						}
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
		engineSrv = s
	})
	return engineSrv
}

// openPoolEngine is postgres.Open plus WithEngine(engineProvider()) when the mode asks for one.
func openPoolEngine(dsn string, opts ...postgres.Option) (*postgres.Pool, error) {
	if sp := engineProvider(); sp != nil {
		opts = append(opts, postgres.WithEngine(sp))
	}
	return postgres.Open(dsn, opts...)
}

// openSQLEngine is sql.Open, bound to the engine of the mode (a Connector WithEngine) when there is one.
//
// A Connector dials direct connections by default (they are not on the engine's
// loop: WithEngine has no effect on them), so the loop path of database/sql is
// run with CELERIS_CONFORMANCE_STREAMING=1, which adds Connector.WithStreaming:
// "1" with no engine mode is the driver's own loop, with an engine mode the
// server's loop (and, on a started epoll or io_uring server, whatever the rule
// of the Connector decides).
func openSQLEngine(dsn string) (*sql.DB, error) {
	sp := engineProvider()
	streaming := os.Getenv("CELERIS_CONFORMANCE_STREAMING") == "1"
	if sp == nil && !streaming {
		return sql.Open(driverName, dsn)
	}
	c, err := postgres.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	if sp != nil {
		c = c.WithEngine(sp)
	}
	if streaming {
		c = c.WithStreaming()
	}
	return sql.OpenDB(c), nil
}
