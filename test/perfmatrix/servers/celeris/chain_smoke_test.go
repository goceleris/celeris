package celeris

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// TestChainSmoke exercises the /chain/* prefixes on the celeris-std-h1
// cell-column: no-auth /chain/api, basic-auth /chain/auth, and a POST
// upload at /chain/fullstack/upload.
func TestChainSmoke(t *testing.T) {
	var target servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == "celeris-std-h1" {
			target = s
			break
		}
	}
	if target == nil {
		t.Fatal("celeris-std-h1 not in registry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := target.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := target.Stop(shutdownCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// /chain/api/json: public, no auth needed.
	resp, err := client.Get(base + "/chain/api/json")
	if err != nil {
		t.Fatalf("GET /chain/api/json: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("/chain/api/json status=%d body=%q", resp.StatusCode, body)
	}

	// /chain/auth/json without auth → 401.
	resp, err = client.Get(base + "/chain/auth/json")
	if err != nil {
		t.Fatalf("GET /chain/auth/json (no auth): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("/chain/auth/json no-auth status=%d, want 401", resp.StatusCode)
	}

	// /chain/auth/json with bench:bench → 200.
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

	// /chain/fullstack/upload with auth — exercises the deepest chain.
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

// TestDriverHandlersSkipWithoutServices asserts that driver routes
// return 503 (not 404) when svcs is nil. The orchestrator counts 503s
// as cell errors and uses this to distinguish missing-service runs
// from router-misconfigured runs.
func TestDriverHandlersSkipWithoutServices(t *testing.T) {
	var target servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == "celeris-std-h1" {
			target = s
			break
		}
	}
	if target == nil {
		t.Fatal("celeris-std-h1 not in registry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := target.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = target.Stop(shutdownCtx)
	}()

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
}
