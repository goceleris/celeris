package redis_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

func setupCelerisRedisEnvAsync(b *testing.B, engineType celeris.EngineType) *celerisRedisEnv {
	b.Helper()
	a := addr(b)
	seedKey(b, a)

	env := &celerisRedisEnv{}
	srv := celeris.New(celeris.Config{
		Engine:        engineType,
		AsyncHandlers: true,
	})
	srv.GET("/bench", func(c *celeris.Context) error {
		v, err := env.client.Get(c.Context(), integratedKey)
		if err != nil {
			return c.String(http.StatusInternalServerError, "get: %v", err)
		}
		return c.String(http.StatusOK, "%s", v)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	opts := []celredis.Option{
		celredis.WithPoolSize(64),
		celredis.WithDialTimeout(5 * time.Second),
		celredis.WithEngine(srv),
	}
	if pw := password(); pw != "" {
		opts = append(opts, celredis.WithPassword(pw))
	}
	c, err := celredis.NewClient(a, opts...)
	if err != nil {
		cancel()
		b.Fatalf("redis: %v", err)
	}

	env.srv = srv
	env.client = c
	env.url = "http://" + srv.Addr().String() + "/bench"
	env.stop = func() {
		_ = c.Close()
		cancel()
	}
	_, _ = io.Discard.Write(nil)
	return env
}

func BenchmarkIntegratedParallel_Celeris_IOUring_Async(b *testing.B) {
	env := setupCelerisRedisEnvAsync(b, celeris.IOUring)
	defer env.stop()
	warmupHTTP(b, keepAliveClient(), env.url, 10)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, env.url)
		}
	})
}

func BenchmarkIntegratedParallel_Celeris_Epoll_Async(b *testing.B) {
	env := setupCelerisRedisEnvAsync(b, celeris.Epoll)
	defer env.stop()
	warmupHTTP(b, keepAliveClient(), env.url, 10)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, env.url)
		}
	})
}
