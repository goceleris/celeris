package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// startLoginServer boots a real celeris server on the given engine with the
// session middleware and the three routes probatorium's
// auth_session_ratelimit refapp exercises in its soak: POST /login (Set +
// Save + JSON body), GET /me (401 without a session, 200 with one) and POST
// /logout (Destroy + 204). It returns the base URL; when the engine is
// unavailable on this host the test is skipped.
func startLoginServer(t *testing.T, engine celeris.EngineType, cfg Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := celeris.New(celeris.Config{Engine: engine})
	srv.Use(New(cfg))
	srv.POST("/login", func(c *celeris.Context) error {
		var req struct {
			Username string `json:"username"`
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil || req.Username == "" {
			return c.JSON(400, map[string]string{"error": "bad request"})
		}
		sess := FromContext(c)
		sess.Set("user", req.Username)
		_ = sess.Save()
		return c.JSON(200, map[string]any{"sid": sess.ID()})
	})
	srv.GET("/me", func(c *celeris.Context) error {
		sess := FromContext(c)
		user := sess.GetString("user")
		if user == "" {
			return c.JSON(401, map[string]string{"error": "unauthenticated"})
		}
		return c.JSON(200, map[string]string{"user": user})
	})
	srv.POST("/logout", func(c *celeris.Context) error {
		_ = FromContext(c).Destroy()
		return c.NoContent(204)
	})

	var startErr atomic.Pointer[error]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if e := srv.StartWithListenerAndContext(ctx, ln); e != nil {
			startErr.Store(&e)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("server goroutine did not exit within 5s")
		}
	})

	// Native engines close the supplied listener and rebind via
	// SO_REUSEPORT, so read srv.Addr() once the engine is up and poll for
	// it to actually accept (mirrors middleware/integration_chain_engine_test).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := startErr.Load(); p != nil {
			msg := (*p).Error()
			if strings.Contains(msg, "io_uring") || strings.Contains(msg, "not available") {
				t.Skipf("engine unavailable on this host: %v", *p)
			}
			t.Fatalf("server start: %v", *p)
		}
		if addr := srv.Addr(); addr != nil {
			a := addr.String()
			if c, derr := net.DialTimeout("tcp", a, 100*time.Millisecond); derr == nil {
				_ = c.Close()
				return "http://" + a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server not ready within 5s")
	return ""
}

// runLoginRoundTrip drives the login -> authenticated GET -> logout flow
// over a real TCP connection using net/http with a cookie jar, exactly as a
// browser (or probatorium's loadgen) would. It is the wire-level oracle for
// the Set-Cookie-before-body fix: the session cookie must reach the client
// on the login response even though the handler wrote a JSON body.
func runLoginRoundTrip(t *testing.T, engine celeris.EngineType) {
	t.Helper()
	base := startLoginServer(t, engine, Config{Store: NewMemoryStore()})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	// Registered after the server's cleanup, so it runs first (LIFO) and
	// the keep-alive connection is gone before the server is asked to stop.
	t.Cleanup(client.CloseIdleConnections)

	// Unauthenticated probe: no cookie yet -> 401 and no Set-Cookie
	// (read-only request under lazy session creation, celeris#487).
	resp, err := client.Get(base + "/me")
	if err != nil {
		t.Fatalf("GET /me (anonymous): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous GET /me: status %d, want 401", resp.StatusCode)
	}
	if sc := resp.Header.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("anonymous GET /me: unexpected Set-Cookie %q", sc)
	}

	// Login: Set + Save + JSON body.
	resp, err = client.Post(base+"/login", "application/json",
		bytes.NewReader([]byte(`{"username":"alice","password":"x"}`)))
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	var login struct {
		SID string `json:"sid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /login: status %d, want 200", resp.StatusCode)
	}
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) != 1 {
		t.Fatalf("POST /login: got %d Set-Cookie headers %q, want exactly 1 (body sid=%s)", len(setCookies), setCookies, login.SID)
	}
	if !strings.HasPrefix(setCookies[0], "celeris_session="+login.SID+";") {
		t.Fatalf("POST /login: Set-Cookie %q does not carry the session id %s from the body", setCookies[0], login.SID)
	}

	// Authenticated GET: the jar replays the cookie -> 200 with the user.
	resp, err = client.Get(base + "/me")
	if err != nil {
		t.Fatalf("GET /me (authenticated): %v", err)
	}
	var me struct {
		User string `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatalf("decode /me body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || me.User != "alice" {
		t.Fatalf("authenticated GET /me: status %d user %q, want 200 alice", resp.StatusCode, me.User)
	}
	if sc := resp.Header.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("authenticated GET /me: unexpected Set-Cookie %q (unmodified session must not re-emit)", sc)
	}

	// Logout: Destroy + 204 -> clearing cookie (Max-Age=0), exactly one.
	resp, err = client.Post(base+"/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("POST /logout: status %d, want 204", resp.StatusCode)
	}
	setCookies = resp.Header.Values("Set-Cookie")
	if len(setCookies) != 1 || !strings.HasPrefix(setCookies[0], "celeris_session=;") || !strings.Contains(setCookies[0], "Max-Age=0") {
		t.Fatalf("POST /logout: Set-Cookie %q, want exactly one clearing cookie", setCookies)
	}

	// The jar dropped the cookie -> anonymous again.
	resp, err = client.Get(base + "/me")
	if err != nil {
		t.Fatalf("GET /me (after logout): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("GET /me after logout: status %d, want 401", resp.StatusCode)
	}
}

// TestLoginRoundTripStdEngine is the portable (every OS) wire-level guard
// for the login shape: sess.Set + sess.Save + c.JSON must yield a
// Set-Cookie that a following request can authenticate with.
func TestLoginRoundTripStdEngine(t *testing.T) {
	runLoginRoundTrip(t, celeris.Std)
}
