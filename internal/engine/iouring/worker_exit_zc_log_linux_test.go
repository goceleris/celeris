//go:build linux

package iouring

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// logSink848 is a slog.Handler that remembers the messages it was asked to log.
type logSink848 struct {
	mu   *sync.Mutex
	msgs *[]string
}

func (h *logSink848) Enabled(context.Context, slog.Level) bool { return true }
func (h *logSink848) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.msgs = append(*h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *logSink848) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logSink848) WithGroup(string) slog.Handler      { return h }

// A worker that ended without returning (celeris#848) and owes a SEND_ZC
// buffer keeps it, but must not call the user's Logger before it is marked
// exited and the pause is notified: a Logger that blocks must not hold up a
// PauseAccept. leaveGroupAfterExit retains the buffers and returns their count;
// reportWorkerExit logs after the notify (the Goexit item from celeris#848 on
// the checklist celeris#884).
func TestGoexitedWorkerKeepsItsZCBuffersWithoutCallingTheLogger848(t *testing.T) {
	var mu sync.Mutex
	var msgs []string
	w := &Worker{
		id:          7,
		listenFD:    -1,
		handoffLoss: &handoffLossStats{},
		logger:      slog.New(&logSink848{mu: &mu, msgs: &msgs}),
		zcHolds:     map[uint64][]zcHold{1: {{sendBuf: make([]byte, 64)}}},
	}
	w.zcHoldCount, w.zcHoldBytes = 1, 64
	before := zcRetainedLen()
	kept := w.leaveGroupAfterExit()
	mu.Lock()
	n := len(msgs)
	mu.Unlock()
	if kept != 1 {
		t.Fatalf("leaveGroupAfterExit kept %d send buffers, want 1", kept)
	}
	if got := zcRetainedLen() - before; got != 1 {
		t.Fatalf("zcRetained grew by %d, want 1", got)
	}
	if n != 0 {
		t.Fatalf("leaveGroupAfterExit called the Logger %d time(s) (%v), before the worker was marked exited", n, msgs)
	}
	w.logKeptZCSendBufs(kept)
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "keeping send buffers") {
		t.Fatalf("logKeptZCSendBufs logged %v, want the one Info record", msgs)
	}
}
