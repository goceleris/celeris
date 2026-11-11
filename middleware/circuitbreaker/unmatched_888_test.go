package circuitbreaker

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestUnmatchedRequestsDoNotCountAgainstTheBreaker888: a breaker mounted with
// Server.Use also sees the requests no route matched (a crawler asking for
// /favicon.ico, a method a path does not take, the automatic OPTIONS answer).
// They reach no downstream, and a 404 is not a failure under the default
// IsError, so while the breaker was HalfOpen (or Open past its cooldown) the
// first of them was taken for the probe, counted a success, and closed a
// breaker whose downstream had not been asked.
func TestUnmatchedRequestsDoNotCountAgainstTheBreaker888(t *testing.T) {
	mw, brk := NewWithBreaker(Config{
		Threshold:      0.5,
		MinRequests:    2,
		WindowSize:     time.Minute,
		CooldownPeriod: 50 * time.Millisecond,
		HalfOpenMax:    1,
	})
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.Use(mw)
	s.GET("/boom", func(*celeris.Context) error { return celeris.NewHTTPError(500, "down") })
	s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("server stopped: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop within 10s")
		}
	}()
	base := "http://" + ln.Addr().String()
	cl := &http.Client{Timeout: 5 * time.Second}
	do := func(method, path string) int {
		t.Helper()
		var resp *http.Response
		var err error
		for deadline := time.Now().Add(5 * time.Second); ; {
			req, _ := http.NewRequest(method, base+path, nil)
			resp, err = cl.Do(req)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if st := do("GET", "/ok"); st != 200 {
		t.Fatalf("GET /ok: %d", st)
	}
	do("GET", "/boom")
	do("GET", "/boom")
	if brk.State() != Open {
		t.Fatalf("after two failures: %s, want open", brk.State())
	}
	time.Sleep(80 * time.Millisecond) // past the cooldown

	for _, rq := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/favicon.ico", 404}, // <unmatched>
		{"DELETE", "/ok", 405},       // <method-not-allowed>
		{"OPTIONS", "/ok", 200},      // <options>
	} {
		if st := do(rq.method, rq.path); st != rq.want {
			t.Errorf("%s %s: %d, want %d", rq.method, rq.path, st, rq.want)
		}
		if got := brk.State(); got != Open {
			t.Fatalf("after %s %s: breaker %s, want it still open (the request reached no downstream)", rq.method, rq.path, got)
		}
	}
	// A request the downstream answers is the probe.
	if st := do("GET", "/ok"); st != 200 {
		t.Errorf("probe GET /ok: %d, want 200", st)
	}
	// The std engine flushes a c.String reply to the client from inside the
	// handler, before the breaker middleware regains control after Next and
	// records the probe's result, so the client can see the 200 and read the
	// state a moment before the HalfOpen -> Closed transition. Wait for it.
	// (A failed probe is not racy this way: the error is returned up the chain
	// and answered after the breaker has settled.) The checks inside the loop
	// above stay immediate: an unmatched request is never counted, so it
	// neither starts nor settles a probe, and the Open state only changes when
	// a counted request arrives.
	deadline := time.Now().Add(5 * time.Second)
	for brk.State() != Closed && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := brk.State(); got != Closed {
		t.Errorf("after a successful probe: %s, want closed", got)
	}
}

// TestCountUnmatchedRefusesFallback888: with CountUnmatched a NotFound handler
// that forwards to the protected downstream is refused while Open.
func TestCountUnmatchedRefusesFallback888(t *testing.T) {
	mw, brk := NewWithBreaker(Config{CountUnmatched: true, MinRequests: 1, Threshold: 0.5, CooldownPeriod: time.Hour})
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.Use(mw)
	s.NotFound(func(c *celeris.Context) error { return celeris.NewHTTPError(502, "upstream down") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.StartWithListenerAndContext(ctx, ln) }()
	get := func() int {
		for deadline := time.Now().Add(5 * time.Second); ; {
			resp, err := http.Get("http://" + ln.Addr().String() + "/legacy")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if st := get(); st != 502 {
		t.Fatalf("first: %d", st)
	}
	if brk.State() != Open {
		t.Fatalf("state %s, want open", brk.State())
	}
	if st := get(); st != 503 {
		t.Errorf("while open: %d, want 503", st)
	}
}
