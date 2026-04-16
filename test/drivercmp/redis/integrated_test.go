//go:build linux

package redis_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	celeris "github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

// ---------------------------------------------------------------------------
// Celeris server helpers
// ---------------------------------------------------------------------------

// celerisRedisEnv holds a running celeris server and Redis client.
type celerisRedisEnv struct {
	srv    *celeris.Server
	client *celredis.Client
	url    string
	stop   func()
}

func setupCelerisRedisEnv(b *testing.B, engineType celeris.EngineType) *celerisRedisEnv {
	b.Helper()
	a := addr(b)
	seedKey(b, a)

	env := &celerisRedisEnv{}
	srv := celeris.New(celeris.Config{
		Engine: engineType,
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
		select {
		case err := <-done:
			cancel()
			b.Fatalf("server exited early: %v", err)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		cancel()
		b.Fatal("server did not bind within deadline")
	}

	opts := []celredis.Option{
		celredis.WithPoolSize(64),
		celredis.WithDialTimeout(5 * time.Second),
		celredis.WithEngine(srv),
	}
	if pw := password(); pw != "" {
		opts = append(opts, celredis.WithPassword(pw))
	}
	client, err := celredis.NewClient(a, opts...)
	if err != nil {
		cancel()
		b.Fatalf("NewClient: %v", err)
	}

	env.srv = srv
	env.client = client
	env.url = "http://" + srv.Addr().String() + "/bench"
	env.stop = func() {
		_ = client.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	return env
}

func keepAliveClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		},
	}
}

func doGet(b *testing.B, client *http.Client, url string) {
	b.Helper()
	resp, err := client.Get(url)
	if err != nil {
		b.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status = %d", resp.StatusCode)
	}
}

func warmupHTTP(b *testing.B, client *http.Client, url string, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		doGet(b, client, url)
	}
}

// seedKey ensures a key exists in Redis for GET benchmarks.
func seedKey(b *testing.B, a string) {
	b.Helper()
	opts := []celredis.Option{
		celredis.WithDialTimeout(5 * time.Second),
	}
	if pw := password(); pw != "" {
		opts = append(opts, celredis.WithPassword(pw))
	}
	c, err := celredis.NewClient(a, opts...)
	if err != nil {
		b.Fatalf("seed NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Set(ctx, benchKeyPrefix+":integrated", benchValue, 0); err != nil {
		b.Fatalf("seed Set: %v", err)
	}
}

const integratedKey = benchKeyPrefix + ":integrated"

// ---------------------------------------------------------------------------
// Celeris integrated (WithEngine) benchmarks
//
// These create a celeris server per invocation. Use -benchtime=Nx (e.g.
// -benchtime=5000x) to avoid repeated server creation during b.N calibration.
// ---------------------------------------------------------------------------

func BenchmarkIntegrated_Celeris_Epoll(b *testing.B) {
	env := setupCelerisRedisEnv(b, celeris.Epoll)
	defer env.stop()

	client := keepAliveClient()
	warmupHTTP(b, client, env.url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, client, env.url)
	}
}

func BenchmarkIntegrated_Celeris_IOUring(b *testing.B) {
	env := setupCelerisRedisEnv(b, celeris.IOUring)
	defer env.stop()

	client := keepAliveClient()
	warmupHTTP(b, client, env.url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, client, env.url)
	}
}

// ---------------------------------------------------------------------------
// net/http + go-redis comparison
// ---------------------------------------------------------------------------

func BenchmarkIntegrated_GoRedis_NetHTTP(b *testing.B) {
	a := addr(b)
	seedKey(b, a)
	ctx := context.Background()

	grOpts := &goredis.Options{
		Addr:         a,
		Password:     password(),
		PoolSize:     64,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	rc := goredis.NewClient(grOpts)
	if err := rc.Ping(ctx).Err(); err != nil {
		_ = rc.Close()
		b.Skipf("go-redis PING: %v", err)
	}
	defer func() { _ = rc.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		v, err := rc.Get(r.Context(), integratedKey).Result()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%s", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	hc := keepAliveClient()
	url := "http://" + ln.Addr().String() + "/bench"
	warmupHTTP(b, hc, url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, hc, url)
	}
}

// ---------------------------------------------------------------------------
// Parallel variants (b.RunParallel)
// ---------------------------------------------------------------------------

func BenchmarkIntegratedParallel_Celeris_Epoll(b *testing.B) {
	env := setupCelerisRedisEnv(b, celeris.Epoll)
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

func BenchmarkIntegratedParallel_Celeris_IOUring(b *testing.B) {
	env := setupCelerisRedisEnv(b, celeris.IOUring)
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

func BenchmarkIntegratedParallel_GoRedis_NetHTTP(b *testing.B) {
	a := addr(b)
	seedKey(b, a)
	ctx := context.Background()

	grOpts := &goredis.Options{
		Addr:         a,
		Password:     password(),
		PoolSize:     64,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	rc := goredis.NewClient(grOpts)
	if err := rc.Ping(ctx).Err(); err != nil {
		_ = rc.Close()
		b.Skipf("go-redis PING: %v", err)
	}
	defer func() { _ = rc.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		v, err := rc.Get(r.Context(), integratedKey).Result()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%s", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	url := "http://" + ln.Addr().String() + "/bench"
	warmupHTTP(b, keepAliveClient(), url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, url)
		}
	})
}
