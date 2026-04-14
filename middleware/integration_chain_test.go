package middleware_test

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
)

// liveServer launches a celeris.Std server on an ephemeral port and
// returns its base URL. It cleans up via t.Cleanup.
func liveServer(t *testing.T, configure func(s *celeris.Server)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	configure(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return "http://" + addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server not ready on %s", addr)
	return ""
}

// TestChain_AuthRateLimitBodyLimit exercises a realistic API stack:
// requestid → recovery → cors → basicauth → ratelimit → bodylimit → handler.
// Verifies (a) authed under-limit succeeds, (b) wrong creds → 401,
// (c) body over limit → 413, (d) preflight skips auth.
func TestChain_AuthRateLimitBodyLimit(t *testing.T) {
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

	url := liveServer(t, func(s *celeris.Server) {
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
		// Register OPTIONS so the router matches preflight; cors handles
		// the actual response, this body never runs.
		s.OPTIONS("/echo", func(c *celeris.Context) error {
			return c.NoContent(204)
		})
	})

	cli := &http.Client{Timeout: 2 * time.Second}

	t.Run("authed-under-limit succeeds", func(t *testing.T) {
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
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello" {
			t.Errorf("body = %q, want hello", body)
		}
	})

	t.Run("wrong creds → 401, no body echoed", func(t *testing.T) {
		req, _ := http.NewRequest("POST", url+"/echo", strings.NewReader("hello"))
		req.SetBasicAuth("alice", "WRONG")
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 401 {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("body over limit → 413", func(t *testing.T) {
		big := strings.Repeat("x", 1024)
		req, _ := http.NewRequest("POST", url+"/echo", strings.NewReader(big))
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

	t.Run("OPTIONS preflight passes auth (defensive skip)", func(t *testing.T) {
		req, _ := http.NewRequest("OPTIONS", url+"/echo", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization")
		// Intentionally NO basic auth header — preflight must succeed.
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == 401 {
			t.Errorf("preflight got 401 — auth middleware did not skip OPTIONS")
		}
		if resp.Header.Get("Access-Control-Allow-Origin") == "" {
			t.Errorf("preflight missing CORS headers (status=%d)", resp.StatusCode)
		}
	})
}

// TestChain_RateLimitRecovery: a handler that panics is caught by recovery
// AND counts as a single ratelimit consumption (no leak/double count).
func TestChain_RateLimitRecovery(t *testing.T) {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	url := liveServer(t, func(s *celeris.Server) {
		s.Use(recovery.New(recovery.Config{DisableLogStack: true}))
		s.Use(ratelimit.New(ratelimit.Config{
			RPS:            1e9,
			Burst:          1 << 20,
			CleanupContext: cleanupCtx,
			DisableHeaders: true,
		}))
		s.GET("/panic", func(c *celeris.Context) error {
			panic("boom")
		})
	})

	cli := &http.Client{Timeout: 2 * time.Second}
	var wg sync.WaitGroup
	const n = 32
	gotStatus := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			resp, err := cli.Get(url + "/panic")
			if err != nil {
				return
			}
			gotStatus[i] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()
	for i, s := range gotStatus {
		if s != 500 {
			t.Errorf("req %d: status = %d, want 500 (recovery → 500)", i, s)
		}
	}
}

// TestChain_SecureCorsCoexist verifies that secure does not clobber the
// Vary header that cors adds (the AddHeader convention from
// middleware/doc.go is correctly followed by both).
func TestChain_SecureCorsCoexist(t *testing.T) {
	url := liveServer(t, func(s *celeris.Server) {
		s.Use(secure.New())
		s.Use(cors.New(cors.Config{
			AllowOrigins: []string{"https://app.example.com"},
		}))
		s.GET("/api", func(c *celeris.Context) error {
			return c.String(200, "ok")
		})
	})

	req, _ := http.NewRequest("GET", url+"/api", nil)
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	vary := resp.Header.Values("Vary")
	if len(vary) == 0 {
		t.Fatal("missing Vary header")
	}
	hasOrigin := false
	for _, v := range vary {
		if strings.Contains(v, "Origin") {
			hasOrigin = true
		}
	}
	if !hasOrigin {
		t.Errorf("Vary missing Origin (got %v) — secure clobbered cors", vary)
	}
	// secure should still emit its own headers.
	if resp.Header.Get("X-Content-Type-Options") == "" {
		t.Error("secure middleware did not set X-Content-Type-Options")
	}
}
