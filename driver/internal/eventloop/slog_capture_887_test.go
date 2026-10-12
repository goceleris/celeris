package eventloop

// The tests of celeris#887 item 18 (worker_exit_887_linux_test.go for the
// Linux workers, portable_exit_887_test.go for the portable one) read what the
// loops log through slog.Default, so they swap the default logger for one that
// records.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// c887Records is a slog.Handler that keeps the records of this package.
type c887Records struct {
	mu   sync.Mutex
	recs []slog.Record
	got  chan struct{}
}

func (h *c887Records) Enabled(context.Context, slog.Level) bool { return true }
func (h *c887Records) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *c887Records) WithGroup(string) slog.Handler            { return h }
func (h *c887Records) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "eventloop") {
		return nil // another package's record
	}
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	select {
	case h.got <- struct{}{}:
	default:
	}
	return nil
}
func (h *c887Records) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// c887Capture makes slog.Default record, and restores it at cleanup.
func c887Capture(t *testing.T) *c887Records {
	t.Helper()
	h := &c887Records{got: make(chan struct{}, 16)}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}
