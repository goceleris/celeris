//go:build linux

package platform

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogPinOutcome(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A plain plan with no failures logs nothing.
	LogPinOutcome(log, "epoll", CPUPlan{CPUs: []int{0, 1}, Allowed: []int{0, 1}}, nil)
	if buf.Len() != 0 {
		t.Fatalf("homogeneous plan logged: %s", buf.String())
	}

	// A heterogeneous plan logs one Info line with the reason.
	h := msr1(t, rng(0, 11))
	p := planWorkerCPUs(12, h.src())
	LogPinOutcome(log, "epoll", p, nil)
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=INFO") ||
		!strings.Contains(out, "little CPUs 2-5") || !strings.Contains(out, "8 of 12 loops pinned") {
		t.Fatalf("heterogeneous plan: want one INFO line naming the little CPUs, got:\n%s", out)
	}

	// Failures log one Warn line, however many loops failed.
	buf.Reset()
	fails := []PinFailure{{CPU: 4, Err: errors.New("invalid argument")}, {CPU: 5, Err: errors.New("invalid argument")}}
	LogPinOutcome(log, "iouring", CPUPlan{CPUs: []int{4, 5, 6, 7}, Allowed: rng(4, 7)}, fails)
	out = buf.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN") ||
		!strings.Contains(out, "failed=2") || !strings.Contains(out, "cpus=4-5") ||
		!strings.Contains(out, "allowed=4-7") || !strings.Contains(out, "invalid argument") {
		t.Fatalf("pin failures: want one WARN line, got:\n%s", out)
	}

	LogPinOutcome(nil, "epoll", p, fails) // a nil logger is not a crash
}

func TestPinFailuresTakeOnce(t *testing.T) {
	var f PinFailures
	k := new(int)
	f.Record(k, 3, errors.New("x"))
	if got, ok := f.Take(k); !ok || got.CPU != 3 {
		t.Fatalf("Take = %+v, %v", got, ok)
	}
	if _, ok := f.Take(k); ok {
		t.Fatal("second Take found the failure again")
	}
}
