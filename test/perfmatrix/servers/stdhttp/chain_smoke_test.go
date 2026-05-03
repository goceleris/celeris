package stdhttp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestChainAPISmoke exercises every chain prefix end-to-end and
// asserts the terminal handler's JSON response comes through the
// middleware stack. Runs with services=nil so the driver routes are
// not touched.
func TestChainAPISmoke(t *testing.T) {
	s := newServer("stdhttp-chain-smoke", modeH1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := s.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
	})

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// /chain/api/json — no auth.
	resp, err := client.Get(base + "/chain/api/json")
	if err != nil {
		t.Fatalf("GET /chain/api/json: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("/chain/api/json status=%d body=%q", resp.StatusCode, body)
	}

	// /chain/auth/json without credentials → 401.
	resp, err = client.Get(base + "/chain/auth/json")
	if err != nil {
		t.Fatalf("GET /chain/auth/json: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("/chain/auth/json no-auth status=%d, want 401", resp.StatusCode)
	}

	// /chain/auth/json with basic auth bench:bench → 200.
	req, _ := http.NewRequest(http.MethodGet, base+"/chain/auth/json", nil)
	req.Header.Set("Authorization", "Basic YmVuY2g6YmVuY2g=")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /chain/auth/json (auth): %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("/chain/auth/json auth status=%d body=%q", resp.StatusCode, body)
	}

	// /chain/security/json with auth → 200.
	req, _ = http.NewRequest(http.MethodGet, base+"/chain/security/json", nil)
	req.Header.Set("Authorization", "Basic YmVuY2g6YmVuY2g=")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /chain/security/json: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/chain/security/json auth status=%d, want 200", resp.StatusCode)
	}

	// /chain/fullstack/upload with auth and body.
	req, _ = http.NewRequest(http.MethodPost, base+"/chain/fullstack/upload",
		strings.NewReader("payload"))
	req.Header.Set("Authorization", "Basic YmVuY2g6YmVuY2g=")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /chain/fullstack/upload: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("/chain/fullstack/upload status=%d body=%q", resp.StatusCode, body)
	}
}

// TestDriverHandlersSkipWithoutServices confirms that when svcs is nil
// (the services=none runner path), driver routes respond 503 rather
// than 404 — the orchestrator relies on this to count errors.
func TestDriverHandlersSkipWithoutServices(t *testing.T) {
	s := newServer("stdhttp-driver-skip", modeH1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := s.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
	})

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/db/user/42", "/cache/demo-key", "/mc/demo-key"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s status=%d, want 503", path, resp.StatusCode)
		}
	}
	resp, err := client.Post(base+"/session", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST /session status=%d, want 503", resp.StatusCode)
	}
}
