//go:build validation

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/goceleris/celeris/validation"
)

// startValidationEndpoint binds the unix socket at
// validation.SocketPath and starts the JSON snapshot server.
// Returns a stop callback that cleanly shuts the listener down and
// removes the socket inode on process exit.
func startValidationEndpoint(logger *slog.Logger) (func(), error) {
	ep, err := validation.StartEndpoint()
	if err != nil {
		return nil, err
	}
	logger.Info("validation endpoint listening",
		"socket", validation.SocketPath,
	)
	return func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ep.Stop(shutCtx); err != nil {
			logger.Warn("validation endpoint stop", "err", err)
		}
	}, nil
}
