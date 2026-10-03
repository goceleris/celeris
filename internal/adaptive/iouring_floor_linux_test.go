//go:build linux

package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
	"github.com/goceleris/celeris/internal/resource"
)

// floorLog682 is a bytes.Buffer an slog handler can write from any goroutine.
type floorLog682 struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *floorLog682) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// fallbackError returns the error of the adaptive engine's "io_uring start
// engine unavailable" record, and whether there was one.
func (l *floorLog682) fallbackError(t *testing.T) (string, bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		var r map[string]any
		if line == "" || json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "io_uring start engine unavailable") {
			err, _ := r["error"].(string)
			return err, true
		}
	}
	return "", false
}

// TestAdaptiveStartOverrideBelowTheIOUringFloor (celeris#682): io_uring needs
// Linux 5.19, so below it the io_uring engine refuses to be built and the
// adaptive engine serves on epoll even when CELERIS_ADAPTIVE_START=iouring
// asks for io_uring, the one setting that made it start on io_uring there.
// Run on the running kernel. Below 5.19 the start engine must be epoll, with
// the fallback record naming the requirement. From 5.19 the floor must not be
// the reason for any fallback: io_uring starts unless something else (no
// io_uring at all, for one) keeps it off. Either way the engine must serve.
func TestAdaptiveStartOverrideBelowTheIOUringFloor(t *testing.T) {
	p := probe.Probe()
	below := p.KernelMajor < 5 || (p.KernelMajor == 5 && p.KernelMinor < 19)
	t.Setenv("CELERIS_ADAPTIVE_START", "iouring")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	var logs floorLog682
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
	}, respHandler{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("adaptive engine did not stop within 10s")
		}
	}()
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	code := getOnce(t, addr)
	active := e.ActiveEngine().Type()
	fallbackErr, fellBack := logs.fallbackError(t)
	t.Logf("celeris682 adaptive kernel=%s (below 5.19: %v) io_uring tier=%v CELERIS_ADAPTIVE_START=iouring: "+
		"start engine %v, GET %d, fallback=%v %q", p.KernelVersion, below, p.IOUringTier, active, code, fellBack, fallbackErr)
	if code != 200 {
		t.Errorf("GET = %d, want 200", code)
	}
	if below {
		if active != engine.Epoll {
			t.Errorf("on kernel %s (before 5.19) the adaptive engine started on %v with CELERIS_ADAPTIVE_START=iouring, "+
				"want epoll: the io_uring engine's cancels fail there with -EINVAL (celeris#682)", p.KernelVersion, active)
		}
		if p.IOUringTier.Available() && (!fellBack || !strings.Contains(fallbackErr, "5.19")) {
			t.Errorf("fallback record %v %q: want the io_uring engine's refusal, naming Linux 5.19", fellBack, fallbackErr)
		}
		return
	}
	if fellBack && strings.Contains(fallbackErr, "5.19") {
		t.Errorf("on kernel %s (5.19 or later) the io_uring start engine was refused by the kernel floor: %q",
			p.KernelVersion, fallbackErr)
	}
}
