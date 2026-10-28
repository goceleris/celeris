//go:build linux

package cache_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/store"
)

// neverSetKV is a store whose Set never returns and ignores its context (a
// remote store whose connection hung); release lets it go at the end.
type neverSetKV struct {
	store.KV
	entered chan struct{}
	release chan struct{}
}

func (s *neverSetKV) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

// TestDefaultSetTimeoutBoundsTheLoopHold888 (celeris#888 item 30): a sync cache
// miss whose Store.Set never returns holds its epoll event loop for as long as
// the bound, and a request that lands on that loop waits as long. The default
// bound is 1 second: the reviewer measured a bystander delayed 951 ms at a 1 s
// bound and 2.95 s at 3 s. The server has one worker, so the bystander is on
// the held loop whatever the kernel does with the connection. Config.SetTimeout
// is left unset: this is the default's test.
//
// The engine needs two workers at least, so bystanders are sent one after the
// other, each on a new connection, until the miss is answered: the first one
// the kernel hands to the held loop waits out the hold, the others are answered
// at once. The longest wait is the one that counts.
func TestDefaultSetTimeoutBoundsTheLoopHold888(t *testing.T) {
	kv := &neverSetKV{KV: store.NewMemoryKV(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	s := celeris.New(celeris.Config{Engine: celeris.Epoll, Workers: 2})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") })
	s.GET("/c", cache.New(cache.Config{Store: kv}), func(c *celeris.Context) error { return c.String(200, "v") })
	started := make(chan error, 1)
	go func() { started <- s.StartWithListener(ln) }()
	t.Cleanup(func() {
		close(kv.release)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		<-started
	})
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(path string) (string, time.Duration, error) {
		began := time.Now()
		r, err := cl.Get("http://" + addr + path)
		if err != nil {
			return "", time.Since(began), err
		}
		defer func() { _ = r.Body.Close() }()
		b, err := io.ReadAll(r.Body)
		return r.Header.Get("X-Cache") + " " + string(b), time.Since(began), err
	}
	// The server is up when /ping answers.
	for deadline := time.Now().Add(30 * time.Second); ; {
		if _, _, err := get("/ping"); err == nil {
			break
		}
		select {
		case err := <-started:
			t.Fatalf("Start returned %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not come up in 30 s")
		}
	}

	type result struct {
		out string
		d   time.Duration
		err error
	}
	miss := make(chan result, 1)
	go func() {
		out, d, err := get("/c")
		miss <- result{out, d, err}
	}()
	select {
	case <-kv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the miss never reached Store.Set")
	}
	var r result
	var ping time.Duration // the longest wait of a bystander
	for answered := false; !answered; {
		select {
		case r = <-miss:
			answered = true
		default:
			if _, d, err := get("/ping"); err != nil {
				t.Fatal(err)
			} else {
				ping = max(ping, d)
			}
		}
	}
	t.Logf("miss answered after %v; the bystander /ping waited %v", r.d.Round(time.Millisecond), ping.Round(time.Millisecond))
	if ping < 300*time.Millisecond {
		t.Fatalf("the longest bystander wait was only %v: no bystander met the held loop, so this test measures nothing", ping)
	}
	const limit = 2500 * time.Millisecond // 1 s bound plus slack; the 5 s placeholder gives about 5 s
	if r.d > limit || ping > limit {
		t.Errorf("miss answered after %v, bystander waited %v; want both within %v (default bound 1 s)", r.d, ping, limit)
	}
	if r.d < 800*time.Millisecond {
		t.Errorf("the miss was answered after %v, before the 1 s default bound", r.d)
	}
}
