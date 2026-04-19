// Package memcached_test benchmarks celeris's Memcached driver against
// github.com/bradfitz/gomemcache. It lives in its own Go module so the
// comparison driver's dependencies do not leak into consumers of celeris.
//
// All benchmarks skip if CELERIS_MEMCACHED_ADDR is unset.
package memcached_test

import (
	"os"
	"strings"
	"testing"
)

const (
	envAddr = "CELERIS_MEMCACHED_ADDR"

	// benchKeyPrefix is the prefix used for benchmark-populated keys. Short
	// values live under <prefix>:k:<index>. The colon is accepted by both
	// drivers because neither treats ':' as a reserved separator (unlike
	// Redis). Memcached disallows whitespace/control bytes inside keys; a
	// colon is fine.
	benchKeyPrefix = "celeris_bench_memcached"
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
