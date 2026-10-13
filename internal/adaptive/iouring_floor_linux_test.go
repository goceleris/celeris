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
	"github.com/goceleris/celeris/internal/engine/iouring"
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

// records returns the log's records, oldest first.
func (l *floorLog682) records() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		var r map[string]any
		if line == "" || json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// fallbackError returns the error of the adaptive engine's "io_uring start
// engine unavailable" record, and whether there was one.
func (l *floorLog682) fallbackError(t *testing.T) (string, bool) {
	t.Helper()
	for _, r := range l.records() {
		if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "io_uring start engine unavailable") {
			err, _ := r["error"].(string)
			return err, true
		}
	}
	return "", false
}

// loggedAnswer682 is the async-cancel-flags probe's answer as the io_uring
// engine's New logged it, which is what decides whether New builds an engine:
//
//   - "rejected": the "async cancel flags rejected by the kernel" record;
//   - "no answer", "unexpected": the Warn (or, before 5.19, Info) records of
//     those answers;
//   - "accepted": the "io_uring engine selected" record (async_cancel_probe),
//     or, on a kernel before 5.19, where New refuses whatever the probe said
//     and logs nothing for an acceptance, the answer its refusal names;
//   - "none": no probe ran, because New refused io_uring earlier (the probed
//     tier is none: io_uring is missing or blocked).
func loggedAnswer682(logs *floorLog682, fallbackErr string) string {
	return answerFromRecords682(logs.records(), fallbackErr)
}

// answerFromRecords682 is loggedAnswer682 on parsed records.
func answerFromRecords682(recs []map[string]any, fallbackErr string) string {
	for _, r := range recs {
		msg, _ := r["msg"].(string)
		switch {
		case strings.HasPrefix(msg, "async cancel flags rejected by the kernel"):
			return "rejected"
		case strings.HasPrefix(msg, "async cancel flags probe got no answer"):
			return "no answer"
		case strings.HasPrefix(msg, "async cancel flags probe got an answer it does not recognise"):
			return "unexpected"
		case msg == "io_uring engine selected":
			if a, _ := r["async_cancel_probe"].(string); a != "" {
				return a
			}
		}
	}
	if _, after, ok := strings.Cut(fallbackErr, "probe result: "); ok {
		for _, a := range []string{"accepted", "rejected", "no answer", "unexpected"} {
			if strings.HasPrefix(after, a) {
				return a
			}
		}
	}
	return "none"
}

// wantFallback682 is the expectation the table rests on: the io_uring start
// engine is refused when the kernel rejected the flags, when no probe ran, and
// on every kernel before 5.19 whatever the probe said, acceptance included.
func wantFallback682(answer string, below bool) bool {
	return answer == "rejected" || answer == "none" || below
}

// TestFloorExpectation682 pins the expectation and the log reading of the
// table above on every host, including the mirror case no CI kernel can run:
// a kernel before 5.19 whose probe ACCEPTED the flags (a partial vendor
// backport) is refused all the same, and a rejection is refused on 5.19+.
func TestFloorExpectation682(t *testing.T) {
	const pre519 = "io_uring sub-engine: io_uring not available on this system: the io_uring engine requires Linux 5.19 or later, " +
		"and kernel 5.15.0 predates it; the IORING_ASYNC_CANCEL flags probe result: "
	cases := []struct {
		name        string
		recs        []map[string]any
		fallbackErr string
		below       bool
		wantAnswer  string
		wantFall    bool
	}{
		{"5.19+, rejected record", []map[string]any{{"msg": "async cancel flags rejected by the kernel: io_uring needs Linux 5.19 or later and is not used (celeris#682)"}}, "x", false, "rejected", true},
		{"5.19+, accepted (selected record)", []map[string]any{{"msg": "io_uring engine selected", "async_cancel_probe": "accepted"}}, "", false, "accepted", false},
		{"5.19+, no answer", []map[string]any{{"msg": "async cancel flags probe got no answer from the kernel: ..."}, {"msg": "io_uring engine selected", "async_cancel_probe": "no answer"}}, "", false, "no answer", false},
		{"5.19+, unexpected", []map[string]any{{"msg": "async cancel flags probe got an answer it does not recognise: ..."}}, "", false, "unexpected", false},
		{"pre-5.19, ACCEPTED (the mirror case)", nil, pre519 + "accepted, which a kernel that carries only some of the flags can give while its other cancel forms fail; use the epoll engine (celeris#682, #872)", true, "accepted", true},
		{"pre-5.19, rejected", []map[string]any{{"msg": "async cancel flags rejected by the kernel: ..."}}, "x", true, "rejected", true},
		{"pre-5.19, no answer", nil, pre519 + "no answer (NewRing failed: EMFILE); use the epoll engine (celeris#682, #872)", true, "no answer", true},
		{"no probe ran (tier none)", nil, "io_uring not available on this system", false, "none", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := answerFromRecords682(tc.recs, tc.fallbackErr); got != tc.wantAnswer {
				t.Errorf("logged answer = %q, want %q", got, tc.wantAnswer)
			}
			if got := wantFallback682(tc.wantAnswer, tc.below); got != tc.wantFall {
				t.Errorf("wantFallback682(%q, below=%v) = %v, want %v", tc.wantAnswer, tc.below, got, tc.wantFall)
			}
		})
	}
}

// newFloorAdaptive682 builds an adaptive engine that is asked to start on
// io_uring (CELERIS_ADAPTIVE_START=iouring, the one setting that skips the
// version check of the start decision) and returns it with its log and
// address, not yet started.
func newFloorAdaptive682(t *testing.T, startEnv string) (*Engine, string, *floorLog682) {
	t.Helper()
	t.Setenv("CELERIS_ADAPTIVE_START", startEnv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	logs := &floorLog682{}
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.NewJSONHandler(logs, nil)),
	}, respHandler{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err)
	}
	return e, addr, logs
}

// listenFloor682 runs Listen, waits for the bind and returns the stop func,
// which cancels Listen, waits for it and shuts the engine down. A Listen that
// ends in an error before it bound is the environment (an io_uring ring that
// cannot be set up beside the rings of the engines this process stopped a
// moment ago, at a small RLIMIT_MEMLOCK): it skips, or fails under
// CELERIS_REQUIRE_UPSWITCH=1, with the engine's own error.
func listenFloor682(t *testing.T, e *Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(25 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		select {
		case lerr := <-done:
			cancel()
			_ = e.Shutdown(context.Background())
			skipOrFailUpswitch662(t, "the adaptive engine did not start here: Listen returned %v before it bound", lerr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if e.Addr() == nil {
		cancel()
		lerr := <-done
		_ = e.Shutdown(context.Background())
		if lerr != nil {
			skipOrFailUpswitch662(t, "the adaptive engine did not bind within 25s here: Listen returned %v once cancelled", lerr)
		}
		t.Fatal("adaptive engine never bound")
	}
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("adaptive engine did not stop within 10s")
		}
		_ = e.Shutdown(context.Background())
	}
}

// TestAdaptiveStartOverrideBelowTheIOUringFloor (celeris#682): the io_uring
// engine refuses to be built where the IORING_ASYNC_CANCEL flags are missing,
// so the adaptive engine serves on epoll even when CELERIS_ADAPTIVE_START=iouring
// asks for io_uring, the one setting that made it start on io_uring there.
//
// What the engine must do is decided by what the probe answered, which is
// what the test reads from the log (loggedAnswer682), and by the kernel's
// floor, never by the kernel's version alone: New refuses io_uring when the
// kernel rejected the flags (on any version), and on every kernel before 5.19
// whatever the probe said (acceptance included, celeris#872). An io_uring
// start engine is expected exactly when neither holds. The refusal's message
// names the 5.19 requirement, so "5.19" in it is no sign of a refusal by the
// version: the old test read it as one on every kernel from 5.19.
//
// The first row asks the kernel it runs on. The others force the probe's
// answer (iouring.ForceAsyncCancelProbe), which is how a kernel that
// accepts the flags is made to reject them: on 5.19 and later, "rejected"
// is the case a regression in the refusal would break, and "accepted" is the
// case a floor that refused too much would break; before 5.19 the "accepted"
// row is the one that has to be refused all the same (celeris#886 item 2).
func TestAdaptiveStartOverrideBelowTheIOUringFloor(t *testing.T) {
	p := probe.Probe()
	below := p.KernelMajor < 5 || (p.KernelMajor == 5 && p.KernelMinor < 19)
	cases := []struct {
		name   string
		force  *iouring.AsyncCancelAnswer
		forced string // the logged answer a forced row must produce
	}{
		{name: "kernel answer"},
		{name: "forced rejected", force: ptr(iouring.AsyncCancelRejected), forced: "rejected"},
		{name: "forced accepted", force: ptr(iouring.AsyncCancelAccepted), forced: "accepted"},
		{name: "forced no answer", force: ptr(iouring.AsyncCancelNoAnswer), forced: "no answer"},
		{name: "forced unexpected", force: ptr(iouring.AsyncCancelUnexpected), forced: "unexpected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.force != nil {
				if !p.IOUringTier.Available() {
					skipOrFailUpswitch662(t, "io_uring unavailable here (tier none): a forced probe answer is never asked for")
				}
				t.Cleanup(iouring.ForceAsyncCancelProbe(*tc.force, "forced by the test"))
			}
			e, addr, logs := newFloorAdaptive682(t, "iouring")
			stop := listenFloor682(t, e)
			defer stop()

			code := getOnce(t, addr)
			active := e.ActiveEngine().Type()
			fallbackErr, fellBack := logs.fallbackError(t)
			answer := loggedAnswer682(logs, fallbackErr)
			if tc.force != nil && answer != tc.forced {
				t.Fatalf("the probe was forced to %q and the log says %q: the forced answer did not reach New", tc.forced, answer)
			}
			wantFallback := wantFallback682(answer, below)
			t.Logf("celeris682 adaptive kernel=%s (below 5.19: %v) io_uring tier=%v CELERIS_ADAPTIVE_START=iouring: "+
				"probe answer %q, start engine %v, GET %d, fallback=%v %q (want a fallback: %v)",
				p.KernelVersion, below, p.IOUringTier, answer, active, code, fellBack, fallbackErr, wantFallback)
			if code != 200 {
				t.Errorf("GET = %d, want 200", code)
			}
			if wantFallback {
				if active != engine.Epoll {
					t.Errorf("probe answer %q on kernel %s (below 5.19: %v): the adaptive engine started on %v with "+
						"CELERIS_ADAPTIVE_START=iouring, want epoll: the io_uring engine is not built there (celeris#682)",
						answer, p.KernelVersion, below, active)
				}
				if !fellBack {
					t.Errorf("probe answer %q on kernel %s: no \"io_uring start engine unavailable\" record, want the io_uring engine's refusal",
						answer, p.KernelVersion)
				}
				if fellBack && !strings.Contains(fallbackErr, "io_uring not available on this system") {
					t.Errorf("fallback error %q: want the io_uring engine's \"not available\" refusal", fallbackErr)
				}
				if fellBack && answer != "none" && !strings.Contains(fallbackErr, "5.19") {
					t.Errorf("fallback error %q: want the refusal to name Linux 5.19", fallbackErr)
				}
				return
			}
			if fellBack {
				if strings.Contains(fallbackErr, "5.19") || strings.Contains(fallbackErr, "IORING_ASYNC_CANCEL") {
					t.Errorf("probe answer %q on kernel %s (5.19 or later): the io_uring start engine was refused for the async-cancel floor: %q",
						answer, p.KernelVersion, fallbackErr)
				}
				// Refused for another reason (RLIMIT_MEMLOCK, a ring that cannot
				// be set up): the environment, not the floor.
				skipOrFailUpswitch662(t, "the io_uring start engine was refused here for a reason other than the floor: %q", fallbackErr)
			}
			if active != engine.IOUring {
				t.Errorf("probe answer %q on kernel %s (5.19 or later): the adaptive engine started on %v with "+
					"CELERIS_ADAPTIVE_START=iouring, want io_uring", answer, p.KernelVersion, active)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
