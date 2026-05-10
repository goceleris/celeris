// Command celeris is a minimal launcher around the celeris HTTP
// server, primarily intended as the entry point for validation soak
// runs. Production deployments should embed celeris.New directly
// rather than depend on this binary.
//
// Under the validation build tag the launcher additionally binds a
// unix-domain socket at /tmp/celeris-validation.sock that streams
// assertion counters as JSON. See main_validation.go and the
// validation package for details.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/goceleris/celeris"
)

func main() {
	addr := flag.String("addr", ":0", "address to listen on (host:port, :0 for OS-assigned)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := celeris.New(celeris.Config{
		Addr:   *addr,
		Logger: logger,
	})
	srv.GET("/", func(c *celeris.Context) error {
		return c.String(200, "celeris ok\n")
	})

	stopValidation, err := startValidationEndpoint(logger)
	if err != nil {
		logger.Error("validation endpoint start failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.StartWithContext(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			logger.Error("server exited", "err", err)
		}
	}

	stopValidation()
}
