package services

import (
	"context"
	"errors"
	"testing"
)

func TestStartReturnsNotImplemented(t *testing.T) {
	h, err := Start(context.Background(), KindPostgres)
	if h != nil {
		t.Fatalf("Start returned non-nil Handles before implementation: %#v", h)
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Start err = %v, want ErrNotImplemented", err)
	}
}

func TestNilHandlesStopIsNoop(t *testing.T) {
	var h *Handles
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("nil Handles.Stop: %v", err)
	}
	if err := h.Seed(context.Background()); err != nil {
		t.Fatalf("nil Handles.Seed: %v", err)
	}
}

func TestKindConstants(t *testing.T) {
	for _, k := range []string{KindPostgres, KindRedis, KindMemcached} {
		if k == "" {
			t.Fatalf("empty Kind constant")
		}
	}
}
