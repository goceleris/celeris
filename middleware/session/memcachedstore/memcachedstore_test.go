package memcachedstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/internal/fakememcached"
	"github.com/goceleris/celeris/middleware/session"
	"github.com/goceleris/celeris/middleware/store"
)

func newTestClient(t *testing.T) (*celmc.Client, *fakememcached.Server) {
	t.Helper()
	srv := fakememcached.Start(t)
	c, err := celmc.NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("celmc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestSetGetDelete(t *testing.T) {
	c, _ := newTestClient(t)
	s := New(c)
	ctx := context.Background()

	if err := s.Set(ctx, "a", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "a")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get: got=%q err=%v", got, err)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: expected ErrNotFound, got %v", err)
	}
}

func TestGetMissingIsErrNotFound(t *testing.T) {
	c, _ := newTestClient(t)
	s := New(c)
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	c, _ := newTestClient(t)
	s := New(c)
	if err := s.Delete(context.Background(), "nope"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestPrefixIsolation(t *testing.T) {
	c, srv := newTestClient(t)
	s := New(c, Options{KeyPrefix: "app1:"})
	_ = s.Set(context.Background(), "k", []byte("v"), 0)
	if v, ok := srv.Data()["app1:k"]; !ok || v != "v" {
		t.Fatalf("expected raw key app1:k, got data=%v", srv.Data())
	}
}

func TestInterfaceConformance(t *testing.T) {
	c, _ := newTestClient(t)
	var _ store.KV = New(c)
}

// celeris#487: cookieless read-only requests must leave the store empty.
func TestCookielessReadOnlyLeavesStoreEmpty(t *testing.T) {
	c, srv := newTestClient(t)
	st := New(c)
	mw := session.New(session.Config{Store: st})
	handler := func(c *celeris.Context) error {
		s := session.FromContext(c)
		_, _ = s.Get("missing")
		return nil
	}
	chain := []celeris.HandlerFunc{mw, handler}

	for range 100 {
		ctx, _ := celeristest.NewContextT(t, "GET", "/", celeristest.WithHandlers(chain...))
		if err := ctx.Next(); err != nil {
			t.Fatal(err)
		}
		for _, h := range ctx.ResponseHeaders() {
			if h[0] == "set-cookie" {
				t.Fatalf("expected no set-cookie header, got %q", h[1])
			}
		}
	}

	if data := srv.Data(); len(data) != 0 {
		t.Fatalf("expected memcached store to be empty, got %d entries: %v", len(data), data)
	}
}
