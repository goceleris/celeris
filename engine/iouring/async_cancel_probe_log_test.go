//go:build linux

package iouring

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/resource"
)

// lockedBuffer is a bytes.Buffer an slog handler can write from any
// goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// records decodes the JSON log lines written so far.
func (l *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// newWarnsWhenTheProbeGetsNoAnswer (celeris#681 R2): New on an engine whose
// async-cancel-flags probe fails before the kernel answers. The reap stays
// off, as for a rejection, but a probe with no answer is not the kernel's
// answer and must not read as one: its record says the probe got no answer,
// names the probe's failure, and on a kernel whose version has the flags
// (5.19 and later) it is a Warn, since the reap is then off where it should
// work. Only the probe-ring seam and API older than round 3 are used here.
func newWarnsWhenTheProbeGetsNoAnswer(t *testing.T) {
	p := probe.Probe()
	want := "INFO"
	if p.KernelMajor > 5 || (p.KernelMajor == 5 && p.KernelMinor >= 19) {
		want = "WARN"
	}
	saved := newAsyncCancelProbeRing
	newAsyncCancelProbeRing = func() (*Ring, error) { return nil, errors.New("celeris681 injected: no ring") }
	cachedAsyncCancel = sync.Once{}
	t.Cleanup(func() {
		newAsyncCancelProbeRing = saved
		cachedAsyncCancel = sync.Once{} // the next New probes the kernel again
	})
	var buf lockedBuffer
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.NewJSONHandler(&buf, nil)),
	}, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	if e.asyncCancelFlags {
		t.Fatal("New turned the hand-off's reap on although the probe got no answer")
	}
	var probeRecs []map[string]any
	for _, r := range buf.records(t) {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "async cancel flags") {
			probeRecs = append(probeRecs, r)
		}
	}
	if len(probeRecs) != 1 {
		t.Fatalf("New logged %d async-cancel-flags probe records, want 1: %v", len(probeRecs), probeRecs)
	}
	r := probeRecs[0]
	msg, _ := r["msg"].(string)
	level, _ := r["level"].(string)
	reason, _ := r["reason"].(string)
	t.Logf("celeris681 probe record on kernel %d.%d: level=%s msg=%q reason=%q", p.KernelMajor, p.KernelMinor,
		level, msg, reason)
	if level != want || !strings.Contains(msg, "no answer") || !strings.Contains(reason, "injected") {
		t.Errorf("a probe that got no answer on kernel %d.%d was logged at %s as %q (reason %q), want %s, saying "+
			"the probe got no answer and naming its failure: a failed probe must not read as the kernel "+
			"rejecting the flags", p.KernelMajor, p.KernelMinor, level, msg, reason, want)
	}
}
