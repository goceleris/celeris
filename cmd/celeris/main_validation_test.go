//go:build validation

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/validation"
)

// TestStartValidationEndpointBindsAndServesJSON exercises the
// launcher's plumbing: starting the endpoint must produce a unix
// socket at validation.SocketPath, serving the JSON snapshot on
// GET / (the legacy alias). The stop callback must remove the
// socket inode so a subsequent run does not collide on the
// "address already in use" path.
func TestStartValidationEndpointBindsAndServesJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	stop, err := startValidationEndpoint(logger)
	if err != nil {
		t.Fatalf("startValidationEndpoint: %v", err)
	}
	t.Cleanup(stop)

	if _, err := os.Stat(validation.SocketPath); err != nil {
		t.Fatalf("socket not present at %s: %v", validation.SocketPath, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", validation.SocketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unused/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want %q", got, "no-store")
	}
	var snap validation.Counters
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	stop()

	if _, err := os.Stat(validation.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("socket still present after stop: stat err=%v", err)
	}
}

// TestStartValidationEndpointSnapshotPath confirms the canonical
// /snapshot path is served too.
func TestStartValidationEndpointSnapshotPath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	stop, err := startValidationEndpoint(logger)
	if err != nil {
		t.Fatalf("startValidationEndpoint: %v", err)
	}
	t.Cleanup(stop)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", validation.SocketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unused/snapshot")
	if err != nil {
		t.Fatalf("GET /snapshot: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var snap validation.Counters
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestStartValidationEndpointSocketPermissions verifies the socket
// inode is chmod'd to 0600 — the snapshot is debug-only and must not
// be readable by other users on a shared host (IM4).
func TestStartValidationEndpointSocketPermissions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	stop, err := startValidationEndpoint(logger)
	if err != nil {
		t.Fatalf("startValidationEndpoint: %v", err)
	}
	t.Cleanup(stop)

	info, err := os.Stat(validation.SocketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode: got %o, want 0600", got)
	}
}
