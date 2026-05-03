package services

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// dockerAvailable reports whether docker is installed and the daemon is
// reachable. Tests that need real containers skip when this returns false.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	cmd := exec.Command("docker", "info", "--format", "{{.ServerVersion}}")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
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

func TestStartNoKindsReturnsEmptyHandles(t *testing.T) {
	h, err := Start(context.Background())
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if h == nil {
		t.Fatalf("Start() returned nil Handles")
	}
	if h.Postgres != nil || h.Redis != nil || h.Memcached != nil {
		t.Fatalf("expected empty Handles, got %+v", h)
	}
	// Stop on an empty Handles is a no-op.
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestKindConstants(t *testing.T) {
	for _, k := range []string{KindPostgres, KindRedis, KindMemcached} {
		if k == "" {
			t.Fatalf("empty Kind constant")
		}
	}
}

func TestStartUnknownKindFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode (docker not required for this test but unify skip policy)")
	}
	if !dockerAvailable(t) {
		t.Skip("Docker not available")
	}
	_, err := Start(context.Background(), "not-a-real-kind")
	if err == nil {
		t.Fatalf("expected error for unknown kind")
	}
}

// TestRoundTripServices boots all three services, seeds the fixture set,
// reads back a handful of known fixtures, and tears down. It is skipped
// when docker is not available or in short mode (CI).
func TestRoundTripServices(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !dockerAvailable(t) {
		t.Skip("Docker not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h, err := Start(ctx, KindPostgres, KindRedis, KindMemcached)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Stop(context.Background()); err != nil {
			t.Logf("Stop: %v", err)
		}
	})

	if err := h.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Postgres round-trip: read a known user.
	pgConn, err := pgx.Connect(ctx, h.Postgres.DSN)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	defer func() { _ = pgConn.Close(ctx) }()
	var name, email string
	var score int
	if err := pgConn.QueryRow(ctx,
		`SELECT name, email, score FROM users WHERE id = $1`, 42,
	).Scan(&name, &email, &score); err != nil {
		t.Fatalf("SELECT users: %v", err)
	}
	if name != "User 42" {
		t.Errorf("name = %q, want %q", name, "User 42")
	}
	if email != "user42@example.com" {
		t.Errorf("email = %q", email)
	}
	if score != 42 {
		t.Errorf("score = %d, want 42", score)
	}

	// Redis round-trip.
	rdb := redis.NewClient(&redis.Options{Addr: h.Redis.Addr})
	defer func() { _ = rdb.Close() }()
	demo, err := rdb.Get(ctx, FixtureDemoKey).Bytes()
	if err != nil {
		t.Fatalf("redis GET demo: %v", err)
	}
	if len(demo) != 4*1024 {
		t.Errorf("redis demo payload = %d bytes, want 4096", len(demo))
	}
	sess, err := rdb.Get(ctx, fmt.Sprintf("user:%d:session", 1)).Bytes()
	if err != nil {
		t.Fatalf("redis GET session: %v", err)
	}
	if len(sess) != 256 {
		t.Errorf("redis session len = %d, want 256", len(sess))
	}

	// Memcached round-trip.
	mc := memcache.New(h.Memcached.Addr)
	item, err := mc.Get(FixtureDemoKey)
	if err != nil {
		t.Fatalf("memcached GET demo: %v", err)
	}
	if len(item.Value) != 4*1024 {
		t.Errorf("memcached demo payload = %d bytes, want 4096", len(item.Value))
	}
	mcItem, err := mc.Get(fmt.Sprintf("user:%d:session", FixtureSessionIDMax))
	if err != nil {
		t.Fatalf("memcached GET session max: %v", err)
	}
	if len(mcItem.Value) != 256 {
		t.Errorf("memcached session len = %d", len(mcItem.Value))
	}
}
