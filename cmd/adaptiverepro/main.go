// Command adaptiverepro is a throwaway repro server for issue #396 (Adaptive
// never promotes to io_uring at 1024c). It serves a trivial get-simple-style
// endpoint on the Adaptive engine; run with CELERIS_ADAPTIVE_DEBUG=1 to emit
// the per-tick controller trace. NOT for commit — deleted after diagnosis.
package main

import (
	"flag"
	"log"
	"net"

	"github.com/goceleris/celeris"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	eng := flag.String("engine", "adaptive", "adaptive|iouring|epoll")
	async := flag.Bool("async", false, "enable AsyncHandlers (per-conn dispatch goroutines) to exercise the #383 async-mode transplant")
	proto := flag.String("protocol", "h1", "h1|h2c|h2c-upgrade|auto")
	flag.Parse()

	et := celeris.Adaptive
	switch *eng {
	case "iouring":
		et = celeris.IOUring
	case "epoll":
		et = celeris.Epoll
	}

	cfg := celeris.Config{
		Engine:        et,
		Protocol:      celeris.HTTP1,
		AsyncHandlers: *async,
	}
	enableUpgrade := true
	switch *proto {
	case "h2c":
		cfg.Protocol = celeris.H2C
	case "h2c-upgrade":
		cfg.Protocol = celeris.HTTP1
		cfg.EnableH2Upgrade = &enableUpgrade
	case "auto":
		cfg.Protocol = celeris.Auto
	}
	srv := celeris.New(cfg)
	srv.GET("/simple", func(c *celeris.Context) error {
		return c.String(200, "ok")
	})

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("adaptiverepro: listen: %v", err)
	}
	log.Printf("ready addr=%s engine=adaptive", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("adaptiverepro: start: %v", err)
	}
}
