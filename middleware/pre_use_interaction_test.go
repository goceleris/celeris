package middleware_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/proxy"
	"github.com/goceleris/celeris/middleware/ratelimit"
)

// TestPreUseInteraction pins the contract that Pre middleware (proxy)
// runs FIRST and sets ClientIP, which Use middleware (ratelimit) then
// observes via its default KeyFunc. If Pre+Use ordering ever broke,
// ratelimit would key on the loopback address and a single noisy peer
// would be invisible.
func TestPreUseInteraction(t *testing.T) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	seenIP := ""
	url := liveServer(t, func(s *celeris.Server) {
		s.Pre(proxy.New(proxy.Config{TrustedProxies: []string{"127.0.0.0/8"}}))
		s.Use(ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 20,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}))
		s.GET("/probe", func(c *celeris.Context) error {
			seenIP = c.ClientIP()
			return c.NoContent(204)
		})
	})

	req, _ := http.NewRequest("GET", url+"/probe", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if seenIP != "203.0.113.99" {
		t.Errorf("ClientIP seen by Use middleware = %q, want 203.0.113.99 — proxy.Pre did not run before Use", seenIP)
	}
}
