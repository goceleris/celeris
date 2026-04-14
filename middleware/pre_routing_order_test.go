package middleware_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/methodoverride"
	"github.com/goceleris/celeris/middleware/proxy"
	"github.com/goceleris/celeris/middleware/redirect"
	"github.com/goceleris/celeris/middleware/rewrite"
)

// startTestServer launches a celeris server on an ephemeral port and
// returns its address + a cleanup func. Uses the std engine so the test
// runs identically on every platform.
func startTestServer(t *testing.T, configure func(s *celeris.Server)) string {
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
	// Wait for the server to be ready.
	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never became ready on %s", addr)
	return ""
}

// noRedirectClient returns an http.Client that does NOT follow redirects,
// so we can inspect Location headers.
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestPreRoutingOrderPin pins the canonical pre-routing order documented in
// middleware/doc.go (proxy → redirect → rewrite → methodoverride). Each
// downstream layer relies on a side-effect of the previous: redirect needs
// proxy's scheme detection; rewrite needs redirect's normalized URL;
// methodoverride needs rewrite's final path. If a future refactor breaks
// the chain, the corresponding assertion below fails with a clear message.
func TestPreRoutingOrderPin(t *testing.T) {
	t.Run("canonical order works end-to-end", func(t *testing.T) {
		captured := struct {
			method, path, scheme, clientIP string
		}{}
		addr := startTestServer(t, func(s *celeris.Server) {
			s.Pre(
				proxy.New(proxy.Config{TrustedProxies: []string{"127.0.0.0/8"}}),
				redirect.HTTPSRedirect(),
				rewrite.New(rewrite.Config{
					Rules: []rewrite.Rule{{Pattern: `^/v1/(.*)$`, Replacement: "/v2/$1"}},
				}),
				methodoverride.New(methodoverride.Config{
					Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
				}),
			)
			s.PUT("/v2/users", func(c *celeris.Context) error {
				captured.method = c.Method()
				captured.path = c.Path()
				captured.scheme = c.Scheme()
				captured.clientIP = c.ClientIP()
				return c.NoContent(204)
			})
		})

		req, _ := http.NewRequest("POST", "http://"+addr+"/v1/users", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.50")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-HTTP-Method-Override", "PUT")
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("status = %d, want 204 (chain misrouted; body=%q)", resp.StatusCode, string(body))
		}
		if captured.method != "PUT" {
			t.Errorf("method = %q, want PUT (methodoverride did not fire — likely placed before rewrite finalized the path)", captured.method)
		}
		if captured.path != "/v2/users" {
			t.Errorf("path = %q, want /v2/users (rewrite did not fire — likely ran after routing)", captured.path)
		}
		if captured.scheme != "https" {
			t.Errorf("scheme = %q, want https (proxy did not set scheme — XFF parsing failed)", captured.scheme)
		}
		if captured.clientIP != "203.0.113.50" {
			t.Errorf("clientIP = %q, want 203.0.113.50 (proxy XFF stripping failed)", captured.clientIP)
		}
	})

	t.Run("rewrite-before-redirect: redirect uses the rewritten path", func(t *testing.T) {
		// Documents the asymmetry: when rewrite runs first, redirect's 301
		// Location reflects the rewritten path, not the original.
		addr := startTestServer(t, func(s *celeris.Server) {
			s.Pre(
				rewrite.New(rewrite.Config{
					Rules: []rewrite.Rule{{Pattern: `^/v1/(.*)$`, Replacement: "/v2/$1"}},
				}),
				redirect.HTTPSRedirect(),
			)
			s.GET("/v2/users", func(c *celeris.Context) error { return c.NoContent(204) })
		})

		req, _ := http.NewRequest("GET", "http://"+addr+"/v1/users", nil)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 301 && resp.StatusCode != 308 {
			t.Fatalf("status = %d, want 3xx redirect", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "/v2/users") {
			t.Errorf("Location = %q; expected to contain rewritten path /v2/users — confirms rewrite ran before redirect saw the URL", loc)
		}
	})

	t.Run("methodoverride-before-rewrite still works (path-independent)", func(t *testing.T) {
		// methodoverride doesn't depend on the final path, so swapping it
		// earlier in the chain is permissible (just not canonical).
		got := ""
		addr := startTestServer(t, func(s *celeris.Server) {
			s.Pre(
				methodoverride.New(methodoverride.Config{
					Getter: methodoverride.HeaderGetter("X-HTTP-Method-Override"),
				}),
				rewrite.New(rewrite.Config{
					Rules: []rewrite.Rule{{Pattern: `^/v1/(.*)$`, Replacement: "/v2/$1"}},
				}),
			)
			s.PUT("/v2/users", func(c *celeris.Context) error {
				got = c.Method()
				return c.NoContent(204)
			})
		})

		req, _ := http.NewRequest("POST", "http://"+addr+"/v1/users", nil)
		req.Header.Set("X-HTTP-Method-Override", "PUT")
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("status = %d, want 204; body=%q", resp.StatusCode, string(body))
		}
		if got != "PUT" {
			t.Errorf("method = %q, want PUT", got)
		}
	})
}
