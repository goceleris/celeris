//go:build memcached_cluster || memcached_cluster_failover

package memcached_test

import (
	"os"
	"strings"
	"testing"
)

const envClusterAddrsShared = "CELERIS_MEMCACHED_CLUSTER_ADDRS"

// clusterAddrsFromEnvShared parses CELERIS_MEMCACHED_CLUSTER_ADDRS.
// Skips the test if unset/empty. Shared between cluster and
// cluster_failover build-tag files.
func clusterAddrsFromEnvShared(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envClusterAddrsShared))
	if raw == "" {
		t.Skipf("skipping: %s not set", envClusterAddrsShared)
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skipf("skipping: %s had no non-empty entries", envClusterAddrsShared)
	}
	return out
}
