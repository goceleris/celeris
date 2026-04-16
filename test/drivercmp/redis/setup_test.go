// Package redis_test benchmarks celeris's Redis driver against go-redis. It
// lives in a dedicated module so the comparison driver does not force its
// dependency graph on consumers of celeris.
//
// All benchmarks skip if CELERIS_REDIS_ADDR is unset.
package redis_test

import (
	"os"
	"strings"
	"testing"
)

const (
	envAddr     = "CELERIS_REDIS_ADDR"
	envPassword = "CELERIS_REDIS_PASSWORD"
)

// benchKeyPrefix is the prefix used for benchmark-populated keys. Short values
// live under <prefix>:k:<index>.
const (
	benchKeyPrefix = "celeris:bench:redis"
	benchKeyCount  = 100
	benchValue     = "hello"
)

// addr returns the benchmark server address or skips.
func addr(b *testing.B) string {
	b.Helper()
	s := strings.TrimSpace(os.Getenv(envAddr))
	if s == "" {
		b.Skipf("skipping: %s not set", envAddr)
	}
	return s
}

// password returns the AUTH password if set. Benchmarks pick it up opaquely —
// it's optional.
func password() string {
	return os.Getenv(envPassword)
}
