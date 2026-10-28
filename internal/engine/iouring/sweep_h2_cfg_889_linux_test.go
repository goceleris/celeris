//go:build linux

package iouring

import (
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// sweepCfg889 is the H2Config the sweep benchmark runs with: the defaults of
// ReadTimeout and ReadHeaderTimeout, so that a request with a body arms its
// deadline. (main has no such fields; the comparison builds main with a copy of
// this file that returns conn.H2Config{}.)
func sweepCfg889() conn.H2Config {
	return conn.H2Config{ReadTimeout: 60 * time.Second, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 600 * time.Second}
}
