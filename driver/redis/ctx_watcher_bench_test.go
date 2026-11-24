package redis_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/redis"
)

// BenchmarkDirectCtx is what a command pays for the context it runs under: the
// same Get, BLPOP (a blocking command, answered at once here) and 10-command
// pipeline, under a context that cannot end (bg), one that can only be canceled
// (cancel), one with a deadline (timeout) and, for Get and BLPOP, a fresh
// cancel-only context made for every command (fresh-cancel, its own allocations
// included). Modes: direct (a Client on an async engine), loop (no engine), direct
// over RESP2, and, on Linux, a started epoll and a started io_uring engine
// (integrated). The contexts of bg, cancel and timeout are made once, so the delta
// is the registration and stop of the watcher, serially (no RunParallel). It uses
// the public API and its own tiny server, so that the same files build against an
// older tag for a comparison:
//
//	go test -c -o base.test ./driver/redis   (in each tree), then run the binaries in turns
func BenchmarkDirectCtx(b *testing.B) {
	addr := benchRESPServer(b)
	modes := []benchMode{
		{"direct", []redis.Option{redis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true}))}},
		{"loop", nil},
		{"direct-resp2", []redis.Option{redis.WithForceRESP2(), redis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std, AsyncHandlers: true}))}},
	}
	modes = append(modes, benchPlatformModes(b)...)
	for _, m := range modes {
		cl, err := redis.NewClient(addr, append([]redis.Option{redis.WithPoolSize(1)}, m.opts...)...)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = cl.Close() })
		ctxs := []struct {
			name string
			ctx  context.Context
		}{
			{"bg", context.Background()},
			{"cancel", mustCtx(context.WithCancel(context.Background()))},
			{"timeout", mustCtx(context.WithTimeout(context.Background(), time.Hour))},
		}
		for _, c := range ctxs {
			b.Run("Get/"+m.name+"/"+c.name, func(b *testing.B) {
				if _, err := cl.Get(c.ctx, "k"); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := cl.Get(c.ctx, "k"); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Blpop/"+m.name+"/"+c.name, func(b *testing.B) {
				if _, err := cl.Do(c.ctx, "BLPOP", "k", "0"); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := cl.Do(c.ctx, "BLPOP", "k", "0"); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Pipeline10/"+m.name+"/"+c.name, func(b *testing.B) {
				run := func() {
					p := cl.Pipeline()
					for j := 0; j < 10; j++ {
						p.Get("k")
					}
					if err := p.Exec(c.ctx); err != nil {
						b.Fatal(err)
					}
				}
				run()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					run()
				}
			})
		}
		b.Run("Get/"+m.name+"/fresh-cancel", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				_, err := cl.Do(ctx, "GET", "k")
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("Blpop/"+m.name+"/fresh-cancel", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				_, err := cl.Do(ctx, "BLPOP", "k", "0")
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func mustCtx(ctx context.Context, cancel context.CancelFunc) context.Context {
	_ = cancel // lives for the whole process
	return ctx
}

// benchRESPServer answers HELLO, PING and GET (the bulk "v"), one reply per
// command, flushed when no more commands are buffered.
func benchRESPServer(b *testing.B) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				w := bufio.NewWriter(c)
				for {
					args, err := benchReadCmd(r)
					if err != nil {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "HELLO":
						_, _ = w.WriteString("%1\r\n$5\r\nproto\r\n:3\r\n")
					case "GET":
						_, _ = w.WriteString("$1\r\nv\r\n")
					default:
						_, _ = w.WriteString("+OK\r\n")
					}
					if r.Buffered() == 0 {
						if w.Flush() != nil {
							return
						}
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func benchReadCmd(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sz, _ := strconv.Atoi(strings.TrimSpace(l[1:]))
		buf := make([]byte, sz+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:sz]))
	}
	return args, nil
}
