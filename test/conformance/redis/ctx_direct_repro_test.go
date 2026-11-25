//go:build redis

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

// Reproducer of celeris#1387, against a real server: a context that ends while a
// command waits on the server. BLPOP on an empty key with timeout 0 blocks until a
// push, i.e. for ever, so only the context can end it. With a Client on an engine
// that runs handlers asynchronously (a direct-mode conn) a cancel() did not end the
// call (it never returned) and a deadline ended it with the raw "i/o timeout",
// which matches neither context.DeadlineExceeded nor context.Canceled.
func TestDirectModeContextEndsABlockingCommand(t *testing.T) {
	addr := addrFromEnv(t)
	modes := map[string][]celredis.Option{
		"no engine (the driver's own loop)": nil,
		"WithEngine(async, direct)":         {celredis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true}))},
	}
	for name, opts := range modes {
		for _, kind := range []string{"deadline", "cancel"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				cl, err := celredis.NewClient(addr, append(append(clientOpts(), celredis.WithPoolSize(1)), opts...)...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cl.Close() }()
				key := fmt.Sprintf("ctxrepro:%d", time.Now().UnixNano())
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.DeadlineExceeded
				if kind == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				} else {
					want = context.Canceled
					ctx, cancel = context.WithCancel(context.Background())
					time.AfterFunc(300*time.Millisecond, cancel)
				}
				defer cancel()
				done := make(chan error, 1)
				start := time.Now()
				go func() { _, err := cl.Do(ctx, "BLPOP", key, "0"); done <- err }()
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("returned %v after %v, want an error that matches %v", err, time.Since(start).Round(time.Millisecond), want)
					}
				case <-time.After(4 * time.Second):
					t.Fatal("BLPOP key 0 did not return 4 s after its context ended (300 ms)")
				}
			})
		}
	}
}
