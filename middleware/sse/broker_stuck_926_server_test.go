//go:build linux || darwin

package sse_test

// celeris#926 on a real server: a subscriber that never reads fills its
// socket, and a request publishes past that subscriber's queue. With
// OnSlowSubscriber returning BrokerPolicyClose or BrokerPolicyRemove, the
// slow path waited for the subscriber's drain goroutine (Close took the
// Client's mutex, held by the blocked write; Remove let the drain write out
// the queue first), so PublishPrepared, and the publishing request, waited
// for the client that does not read. The publishing request must be answered
// within 5 s. BrokerPolicyDrop and a subscriber that reads are the controls.
// (Reviewer probe of celeris#923's family audit, made a test.)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

func TestBrokerPublisherDoesNotWaitForAStuckSubscriber926(t *testing.T) {
	engines := []struct {
		name string
		eng  celeris.EngineType
	}{{"std", celeris.Std}}
	if runtime.GOOS == "linux" {
		engines = append(engines, struct {
			name string
			eng  celeris.EngineType
		}{"epoll", celeris.Epoll})
	}
	payload := strings.Repeat("x", 64<<10)
	for _, e := range engines {
		for _, reader := range []string{"never-reads", "reads"} {
			for _, pol := range []struct {
				name string
				p    sse.BrokerPolicy
			}{{"drop", sse.BrokerPolicyDrop}, {"remove", sse.BrokerPolicyRemove}, {"close", sse.BrokerPolicyClose}} {
				t.Run(e.name+"/"+reader+"/"+pol.name, func(t *testing.T) {
					broker := sse.NewBroker(sse.BrokerConfig{
						SubscriberBuffer: 4,
						OnSlowSubscriber: func(*sse.Client, *sse.PreparedEvent) sse.BrokerPolicy { return pol.p },
					})
					subscribed := make(chan struct{}, 1)
					addr := startServer926(t, e.eng, func(s *celeris.Server) {
						s.GET("/events", sse.New(sse.Config{Handler: func(c *sse.Client) {
							unsub := broker.Subscribe(c)
							defer unsub()
							subscribed <- struct{}{}
							<-c.Context().Done()
						}}))
						s.GET("/pub", func(c *celeris.Context) error {
							// 16 MiB, paced so that a subscriber that does not read has
							// filled its socket buffers (and its drain's write blocks)
							// before its queue overflows: the state of a stuck client.
							for i := 0; i < 256; i++ {
								broker.Publish(sse.Event{Data: payload})
								time.Sleep(500 * time.Microsecond)
							}
							return c.String(200, "published")
						})
					})
					// The slow subscriber: sends its request, never reads.
					conn, err := net.Dial("tcp", addr)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.Close() }()
					if tc, ok := conn.(*net.TCPConn); ok && reader == "never-reads" {
						_ = tc.SetReadBuffer(4096)
					}
					if _, err := fmt.Fprintf(conn, "GET /events HTTP/1.1\r\nHost: x\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					if reader == "reads" {
						go func() { _, _ = io.Copy(io.Discard, conn) }()
					}
					select {
					case <-subscribed:
					case <-time.After(5 * time.Second):
						t.Fatal("subscriber never subscribed")
					}
					cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
					start := time.Now()
					resp, err := cl.Get("http://" + addr + "/pub")
					took := time.Since(start)
					status := 0
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						status = resp.StatusCode
					}
					ping, perr := cl.Get("http://" + addr + "/ping")
					pst := 0
					if perr == nil {
						_ = ping.Body.Close()
						pst = ping.StatusCode
					}
					t.Logf("engine=%s reader=%s policy=%s publish: status=%d err=%v took=%v; /ping after: %d %v", e.name, reader, pol.name, status, err, took.Round(time.Millisecond), pst, perr)
					if err != nil {
						t.Errorf("the publishing request got no response within 5 s (%v): it waits for the slow subscriber's client", err)
					}
					_ = conn.Close() // let the subscriber's writes fail so the server can stop
				})
			}
		}
	}
}

func startServer926(t *testing.T, eng celeris.EngineType, routes func(*celeris.Server)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	s := celeris.New(celeris.Config{Engine: eng, Addr: addr})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(200, "ok") })
	routes(s)
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start() }()
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !ready; {
		select {
		case err := <-startDone:
			if err == nil {
				err = errors.New("Start returned nil")
			}
			t.Fatalf("server did not start: %v", err)
		default:
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_ = resp.Body.Close()
			ready = resp.StatusCode == 200
		}
		if !ready {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("server not ready")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		select {
		case <-startDone:
		case <-time.After(15 * time.Second):
			t.Errorf("Start did not return within 15 s of Shutdown")
		}
	})
	return addr
}
