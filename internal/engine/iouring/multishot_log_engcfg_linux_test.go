//go:build linux

package iouring

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// Checklist #884 item 48 (planned as 47). The start-up line "io_uring engine
// selected" printed the tier's capabilities, multishot_recv=true and
// provided_buffers=true on every high-tier kernel, while the provided-buffer
// ring (Worker.bufRing) is created only with CELERIS_IOURING_MULTISHOT_RECV=1.
// A reader of a default run's log took the line for "the ring is in use".
//
// The line now reports what the engine does (as fixed_files does since
// celeris#541): with the variable unset or anything but "1" both fields are
// false, with "1" they follow the tier. The worker's ring must agree with the
// line, whichever way the two are decided.
func TestStartupLineReportsTheMultishotRecvThatIsInUse(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		enabled   bool
	}{
		{"unset", "", false},
		{"zero", "0", false},
		{"true-is-not-1", "true", false},
		{"one", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", tc.env)
			var log lockedBuf
			e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
				return New(resource.Config{
					Addr:      "127.0.0.1:0",
					Protocol:  engine.HTTP1,
					Resources: resource.Resources{Workers: 2},
					Logger:    slog.New(slog.NewJSONHandler(&log, nil)),
				}, idleRespHandler662{})
			})
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("engine did not stop within 10s")
				}
			})
			if !e.tier.SupportsMultishotRecv() {
				t.Skipf("tier %s has no multishot recv: both fields are false whatever the variable says", e.tier.Tier())
			}
			t.Logf("tier %s, CELERIS_IOURING_MULTISHOT_RECV=%q", e.tier.Tier(), tc.env)

			var rec map[string]any
			for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
				var r map[string]any
				if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "io_uring engine selected" {
					rec = r
				}
			}
			if rec == nil {
				t.Fatalf("no \"io_uring engine selected\" record in:\n%s", log.String())
			}
			if got := rec["multishot_recv"]; got != tc.enabled {
				t.Errorf("multishot_recv = %v, want %v: the line must say whether multishot recv is in use", got, tc.enabled)
			}
			if got := rec["provided_buffers"]; got != tc.enabled {
				t.Errorf("provided_buffers = %v, want %v: the ring is mapped only with CELERIS_IOURING_MULTISHOT_RECV=1", got, tc.enabled)
			}
			for _, w := range workers662(e) {
				if has := w.bufRing != nil; has != tc.enabled {
					t.Errorf("worker %d has a provided-buffer ring = %v, want %v", w.id, has, tc.enabled)
				}
			}
		})
	}
}

// TestMultishotRecvDecidedOnceAtNew pins that the start-up line and the
// workers' rings read the opt-in once: New captures it, and a change of the
// environment between New and the workers' start (a test can do that; an
// operator cannot) reaches neither. Without the capture the line says what New
// saw and the ring what run saw. Review round 1 nit on the checklist #884
// item 48 change.
func TestMultishotRecvDecidedOnceAtNew(t *testing.T) {
	for _, tc := range []struct {
		name, atNew, afterNew string
		enabled               bool
	}{
		{"on at New, off after", "1", "", true},
		{"off at New, on after", "", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log lockedBuf
			e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
				t.Setenv(envMultishotRecv, tc.atNew)
				e, err := New(resource.Config{
					Addr:      "127.0.0.1:0",
					Protocol:  engine.HTTP1,
					Resources: resource.Resources{Workers: 2},
					Logger:    slog.New(slog.NewJSONHandler(&log, nil)),
				}, idleRespHandler662{})
				t.Setenv(envMultishotRecv, tc.afterNew)
				return e, err
			})
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("engine did not stop within 10s")
				}
			})
			if !e.tier.SupportsMultishotRecv() {
				t.Skipf("tier %s has no multishot recv: nothing to decide", e.tier.Tier())
			}
			var rec map[string]any
			for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
				var r map[string]any
				if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "io_uring engine selected" {
					rec = r
				}
			}
			if rec == nil {
				t.Fatalf("no \"io_uring engine selected\" record in:\n%s", log.String())
			}
			if got := rec["multishot_recv"]; got != tc.enabled {
				t.Errorf("multishot_recv = %v, want %v (the value at New)", got, tc.enabled)
			}
			for _, w := range workers662(e) {
				if has := w.bufRing != nil; has != tc.enabled {
					t.Errorf("worker %d has a provided-buffer ring = %v, want %v: the line and the ring must agree", w.id, has, tc.enabled)
				}
			}
		})
	}
}
