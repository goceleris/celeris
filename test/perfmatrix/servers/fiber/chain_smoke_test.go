package fiber

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestChainAPISmoke(t *testing.T) {
	s := New()
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

	resp, err := client.Get(base + "/chain/api/json")
	if err != nil {
		t.Fatalf("GET /chain/api/json: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("/chain/api/json status=%d body=%q", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/chain/fullstack/upload",
		strings.NewReader("payload"))
	req.Header.Set("Authorization", "Basic YmVuY2g6YmVuY2g=")
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

func TestDriverHandlersSkipWithoutServices(t *testing.T) {
	s := New()
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
	resp, err := client.Get(base + "/db/user/42")
	if err != nil {
		t.Fatalf("GET /db/user/42: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /db/user/42 status=%d, want 503", resp.StatusCode)
	}
}
