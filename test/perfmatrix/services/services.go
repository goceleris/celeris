// Package services owns the lifecycle of the external datastores that the
// perfmatrix driver scenarios talk to (Postgres, Redis, Memcached). Each
// service is started in a Docker container on the same host as the
// orchestrator, probed until ready, seeded with a known fixture set, and
// torn down when the orchestrator exits.
//
// Only "local" (Docker on same host) and "none" (skip) modes are
// supported. The mage perfmatrix targets are responsible for ensuring
// the orchestrator binary runs on the correct host (msr1 or developer
// workstation) before invoking Start.
package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// Fixture IDs exposed to wave-3 driver handlers so they can generate
// realistic request traffic against the seeded data set.
const (
	FixtureUserMinID    = 1
	FixtureUserMaxID    = 10000
	FixtureDemoKey      = "demo-key"
	FixtureSessionIDMin = 1
	FixtureSessionIDMax = 1000
)

// Kind enumerates the services perfmatrix can provision. String values are
// accepted by Start to select which subset to bring up.
const (
	KindPostgres  = "postgres"
	KindRedis     = "redis"
	KindMemcached = "memcached"
)

// Container image tags. Pinned so runs are reproducible.
const (
	imagePostgres  = "postgres:17-alpine"
	imageRedis     = "redis:8.2-alpine"
	imageMemcached = "memcached:1.6.41-alpine"
)

// Handles is the set of running services returned by Start. Fields are nil
// when the corresponding service was not requested. Driver scenarios read
// DSNs/addresses off the Handles to configure their clients.
type Handles struct {
	Postgres  *PGService
	Redis     *RedisService
	Memcached *MCService
}

// PGService describes a running Postgres instance.
type PGService struct {
	// DSN is a libpq connection string, e.g.
	// "postgres://bench:bench@127.0.0.1:54321/bench?sslmode=disable".
	DSN string
	// ContainerID is the docker container ID. Empty when running without
	// docker (unit tests).
	ContainerID string
}

// RedisService describes a running Redis instance.
type RedisService struct {
	// Addr is "host:port".
	Addr        string
	ContainerID string
}

// MCService describes a running Memcached instance.
type MCService struct {
	// Addr is "host:port".
	Addr        string
	ContainerID string
}

// Start provisions every service named in kinds (see the Kind* constants).
// It waits until each service is accepting connections before returning.
// Pass zero kinds to skip provisioning entirely; the returned Handles will
// have all fields nil.
//
// All containers are started with --rm so they clean up on docker daemon
// restart. Ports are bound to 127.0.0.1 only so test services never leak
// to the network.
func Start(ctx context.Context, kinds ...string) (*Handles, error) {
	h := &Handles{}
	if len(kinds) == 0 {
		return h, nil
	}

	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("perfmatrix/services: docker not available: %w", err)
	}

	for _, k := range kinds {
		switch k {
		case KindPostgres:
			pg, err := startPostgres(ctx)
			if err != nil {
				_ = h.Stop(context.Background())
				return nil, fmt.Errorf("start postgres: %w", err)
			}
			h.Postgres = pg
		case KindRedis:
			rs, err := startRedis(ctx)
			if err != nil {
				_ = h.Stop(context.Background())
				return nil, fmt.Errorf("start redis: %w", err)
			}
			h.Redis = rs
		case KindMemcached:
			mc, err := startMemcached(ctx)
			if err != nil {
				_ = h.Stop(context.Background())
				return nil, fmt.Errorf("start memcached: %w", err)
			}
			h.Memcached = mc
		default:
			_ = h.Stop(context.Background())
			return nil, fmt.Errorf("perfmatrix/services: unknown kind %q", k)
		}
	}

	return h, nil
}

// Stop tears down every provisioned service. It is safe to call with a nil
// receiver (no-op).
func (h *Handles) Stop(ctx context.Context) error {
	if h == nil {
		return nil
	}
	var firstErr error
	if h.Postgres != nil && h.Postgres.ContainerID != "" {
		if err := dockerKill(ctx, h.Postgres.ContainerID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if h.Redis != nil && h.Redis.ContainerID != "" {
		if err := dockerKill(ctx, h.Redis.ContainerID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if h.Memcached != nil && h.Memcached.ContainerID != "" {
		if err := dockerKill(ctx, h.Memcached.ContainerID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Seed loads the fixture set into every provisioned service:
//
//   - Postgres: users table (id, name, email, score) with 10 000 rows.
//   - Redis: demo-key = 4 KiB payload; user:<id>:session = 256-byte blob
//     for id ∈ [1, 1000].
//   - Memcached: demo-key = 4 KiB payload; user:<id>:session = 256-byte
//     blob for id ∈ [1, 1000].
//
// Safe to call with a nil receiver (no-op).
func (h *Handles) Seed(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if h.Postgres != nil {
		if err := seedPostgres(ctx, h.Postgres.DSN); err != nil {
			return fmt.Errorf("seed postgres: %w", err)
		}
	}
	if h.Redis != nil {
		if err := seedRedis(ctx, h.Redis.Addr); err != nil {
			return fmt.Errorf("seed redis: %w", err)
		}
	}
	if h.Memcached != nil {
		if err := seedMemcached(h.Memcached.Addr); err != nil {
			return fmt.Errorf("seed memcached: %w", err)
		}
	}
	return nil
}

// startPostgres runs a Postgres container on a random loopback port and
// waits for it to finish initialising. The returned PGService carries the
// container id so Stop can tear it down.
func startPostgres(ctx context.Context) (*PGService, error) {
	id, err := dockerRun(ctx,
		"-e", "POSTGRES_USER=bench",
		"-e", "POSTGRES_PASSWORD=bench",
		"-e", "POSTGRES_DB=bench",
		"-p", "127.0.0.1:0:5432/tcp",
		imagePostgres,
	)
	if err != nil {
		return nil, err
	}
	port, err := dockerPort(ctx, id, "5432/tcp")
	if err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	// Wait for initdb + "database system is ready to accept connections".
	if err := waitForLogLine(ctx, id, "database system is ready to accept connections", 45*time.Second); err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	// Postgres logs "ready" twice (initdb bootstrap + real start). Probe
	// a real connection before declaring the service up so seed code
	// doesn't race the listener.
	dsn := fmt.Sprintf("postgres://bench:bench@127.0.0.1:%d/bench?sslmode=disable", port)
	if err := waitForPostgres(ctx, dsn, 30*time.Second); err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	return &PGService{DSN: dsn, ContainerID: id}, nil
}

// startRedis runs a Redis container and waits for it to accept connections.
func startRedis(ctx context.Context) (*RedisService, error) {
	id, err := dockerRun(ctx,
		"-p", "127.0.0.1:0:6379/tcp",
		imageRedis,
	)
	if err != nil {
		return nil, err
	}
	port, err := dockerPort(ctx, id, "6379/tcp")
	if err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	if err := waitForLogLine(ctx, id, "Ready to accept connections", 30*time.Second); err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitForTCP(ctx, addr, 15*time.Second); err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	return &RedisService{Addr: addr, ContainerID: id}, nil
}

// startMemcached runs a memcached container. memcached is silent after
// binding so we only TCP-probe the exposed port.
func startMemcached(ctx context.Context) (*MCService, error) {
	id, err := dockerRun(ctx,
		"-p", "127.0.0.1:0:11211/tcp",
		imageMemcached,
	)
	if err != nil {
		return nil, err
	}
	port, err := dockerPort(ctx, id, "11211/tcp")
	if err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitForTCP(ctx, addr, 15*time.Second); err != nil {
		_ = dockerKill(context.Background(), id)
		return nil, err
	}
	return &MCService{Addr: addr, ContainerID: id}, nil
}

// dockerRun executes `docker run -d --rm <extraArgs...> <image>` and
// returns the container ID. extraArgs must include -p bindings; the
// caller is responsible for image selection.
func dockerRun(ctx context.Context, extraArgs ...string) (string, error) {
	args := append([]string{"run", "-d", "--rm"}, extraArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("docker run: empty container id")
	}
	return id, nil
}

// dockerPort returns the host port that `containerPort` (e.g. "5432/tcp")
// is mapped to. Retries briefly — docker sometimes reports 0 immediately
// after `run -d` returns.
func dockerPort(ctx context.Context, id, containerPort string) (int, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd := exec.CommandContext(ctx, "docker", "port", id, containerPort)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			// Output shape: "0.0.0.0:54321\n127.0.0.1:54321\n" — look for
			// the 127.0.0.1 line since we bound to loopback only.
			for _, line := range strings.Split(out.String(), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// Accept either "127.0.0.1:PORT" or "0.0.0.0:PORT" — pick the
				// one that has a real port.
				idx := strings.LastIndex(line, ":")
				if idx < 0 {
					continue
				}
				port := 0
				if _, perr := fmt.Sscanf(line[idx+1:], "%d", &port); perr == nil && port > 0 {
					return port, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("docker port %s %s: no mapping found", id, containerPort)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dockerKill sends SIGKILL to the named container with a 5-second
// timeout.
func dockerKill(ctx context.Context, id string) error {
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(kctx, "docker", "kill", id)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Non-fatal: containers started with --rm may already be gone.
		return fmt.Errorf("docker kill %s: %w: %s", id, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// waitForLogLine tails `docker logs -f <id>` until `needle` appears or
// the timeout elapses.
func waitForLogLine(ctx context.Context, id, needle string, timeout time.Duration) error {
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(lctx, "docker", "logs", "-f", id)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	found := make(chan struct{}, 1)
	go scanForNeedle(stdout, needle, found)
	go scanForNeedle(stderr, needle, found)

	select {
	case <-found:
		return nil
	case <-lctx.Done():
		return fmt.Errorf("waiting for %q in %s logs: %w", needle, id, lctx.Err())
	}
}

// scanForNeedle reads r line-by-line and signals done once `needle`
// appears.
func scanForNeedle(r io.Reader, needle string, done chan<- struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), needle) {
			select {
			case done <- struct{}{}:
			default:
			}
			return
		}
	}
}

// waitForTCP dials addr with a 50 ms backoff until it succeeds or the
// deadline elapses.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backoff := 50 * time.Millisecond
	var d net.Dialer
	for {
		conn, err := d.DialContext(wctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("tcp probe %s: %w", addr, wctx.Err())
		case <-time.After(backoff):
		}
	}
}

// waitForPostgres rolls a real libpq connection + SELECT 1 until the
// server is ready or the deadline elapses.
func waitForPostgres(ctx context.Context, dsn string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	backoff := 100 * time.Millisecond
	for {
		conn, err := pgx.Connect(wctx, dsn)
		if err == nil {
			var n int
			qerr := conn.QueryRow(wctx, "SELECT 1").Scan(&n)
			_ = conn.Close(wctx)
			if qerr == nil && n == 1 {
				return nil
			}
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("postgres probe %s: %w", dsn, wctx.Err())
		case <-time.After(backoff):
		}
	}
}

// seedPostgres creates the users table and COPYs 10 000 rows.
func seedPostgres(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS users`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE users (
		id    INT PRIMARY KEY,
		name  TEXT NOT NULL,
		email TEXT NOT NULL,
		score INT  NOT NULL
	)`); err != nil {
		return err
	}

	rows := make([][]any, 0, FixtureUserMaxID-FixtureUserMinID+1)
	for i := FixtureUserMinID; i <= FixtureUserMaxID; i++ {
		rows = append(rows, []any{
			i,
			fmt.Sprintf("User %d", i),
			fmt.Sprintf("user%d@example.com", i),
			i % 100,
		})
	}
	_, err = conn.CopyFrom(ctx, pgx.Identifier{"users"},
		[]string{"id", "name", "email", "score"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// seedRedis writes the demo-key + per-user session blobs into Redis.
func seedRedis(ctx context.Context, addr string) error {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = rdb.Close() }()

	payload := bytes.Repeat([]byte("a"), 4*1024)
	if err := rdb.Set(ctx, FixtureDemoKey, payload, 0).Err(); err != nil {
		return err
	}

	blob := bytes.Repeat([]byte("s"), 256)
	for i := FixtureSessionIDMin; i <= FixtureSessionIDMax; i++ {
		key := fmt.Sprintf("user:%d:session", i)
		if err := rdb.Set(ctx, key, blob, 0).Err(); err != nil {
			return err
		}
	}
	return nil
}

// seedMemcached writes the demo-key + per-user session blobs into
// memcached. gomemcache has no context API so ctx is accepted only for
// symmetry.
func seedMemcached(addr string) error {
	mc := memcache.New(addr)

	payload := bytes.Repeat([]byte("a"), 4*1024)
	if err := mc.Set(&memcache.Item{Key: FixtureDemoKey, Value: payload}); err != nil {
		return err
	}

	blob := bytes.Repeat([]byte("s"), 256)
	for i := FixtureSessionIDMin; i <= FixtureSessionIDMax; i++ {
		key := fmt.Sprintf("user:%d:session", i)
		if err := mc.Set(&memcache.Item{Key: key, Value: blob}); err != nil {
			return err
		}
	}
	return nil
}
