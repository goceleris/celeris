//go:build linux

// Engine-matrix variant of the cross-middleware integration tests. These
// run against the native epoll engine on Linux to catch middleware ↔ engine
// regressions that would never surface with the std engine bridge.
package middleware_test

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
)

// liveEngineServer is the epoll equivalent of liveServer (which uses Std).
// Same shape, different engine — surfaces engine-specific bugs in the
// middleware contract.
//
// Native engines close the supplied listener and rebind via SO_REUSEPORT,
// so we must read s.Addr() AFTER the engine is up rather than trusting
// the original ln.Addr(). The dial loop polls both for the engine to
// publish its address and for that address to actually accept connections.
func liveEngineServer(t *testing.T, eng celeris.EngineType, configure func(s *celeris.Server)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := celeris.New(celeris.Config{Engine: eng})
	configure(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.Addr(); addr != nil {
			a := addr.String()
			c, derr := net.DialTimeout("tcp", a, 100*time.Millisecond)
			if derr == nil {
				_ = c.Close()
				return "http://" + a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server not ready within 5s")
	return ""
}

// TestEngineMatrix_AuthRateLimitBodyLimit pins the same chain as
// TestChain_AuthRateLimitBodyLimit but executes it against the native
// epoll engine. Catches engine-specific failures (body parsing, header
// materialization, response flushing) that would never surface with the
// std net/http bridge.
func TestEngineMatrix_AuthRateLimitBodyLimit(t *testing.T) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sha256Verify := func(stored, pass string) bool {
		want, err := hex.DecodeString(stored)
		if err != nil {
			return false
		}
		got := sha256.Sum256([]byte(pass))
		return subtle.ConstantTimeCompare(got[:], want) == 1
	}
	hashPw := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}

	// Some CI sandboxes restrict raw sockets / SO_REUSEPORT — skip the
	// epoll matrix on those rather than flaking. The test below wires
	// real-server lifecycle, so a hard "engine not ready" is the
	// signal to skip.
	if testing.Short() {
		t.Skip("engine matrix test skipped in -short")
	}

	url := liveEngineServer(t, celeris.Epoll, func(s *celeris.Server) {
		s.Use(requestid.New())
		s.Use(recovery.New())
		s.Use(cors.New(cors.Config{
			AllowOrigins: []string{"https://app.example.com"},
		}))
		s.Use(basicauth.New(basicauth.Config{
			HashedUsers:     map[string]string{"alice": hashPw("s3cr3t")},
			HashedUsersFunc: sha256Verify,
		}))
		s.Use(ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 20,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}))
		s.Use(bodylimit.New(bodylimit.Config{Limit: "256B"}))
		s.POST("/echo", func(c *celeris.Context) error {
			return c.Blob(200, "text/plain", c.Body())
		})
		s.OPTIONS("/echo", func(c *celeris.Context) error { return c.NoContent(204) })
	})

	cli := &http.Client{Timeout: 2 * time.Second}

	t.Run("authed-under-limit on epoll", func(t *testing.T) {
		req, _ := http.NewRequest("POST", url+"/echo", strings.NewReader("hello"))
		req.SetBasicAuth("alice", "s3cr3t")
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("body over limit on epoll → 413", func(t *testing.T) {
		req, _ := http.NewRequest("POST", url+"/echo", strings.NewReader(strings.Repeat("x", 1024)))
		req.SetBasicAuth("alice", "s3cr3t")
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 413 {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("OPTIONS preflight on epoll → CORS", func(t *testing.T) {
		req, _ := http.NewRequest("OPTIONS", url+"/echo", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization")
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == 401 {
			t.Errorf("preflight got 401 — auth middleware did not skip OPTIONS on epoll")
		}
		if resp.Header.Get("Access-Control-Allow-Origin") == "" {
			t.Errorf("preflight missing CORS headers on epoll (status=%d)", resp.StatusCode)
		}
	})
}
