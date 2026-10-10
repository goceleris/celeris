package celeris_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#949: ToHandler gave the celeris handler a stream context of its
// own, not the request's, so a client that went away (or an
// http.TimeoutHandler above it) never reached a handler waiting on
// c.Context(). c.Context() now ends with r.Context(), on HTTP/1 and HTTP/2:
// the net/http contract for a handler (and the one the net/http routers and
// middleware ToHandler is for rely on); unlike the engines' own HTTP/1
// streams, ToHandler has no engine to agree with.

type toHandlerOutcome949 struct {
	ended   bool
	err     error
	started time.Time
	at      time.Time
}

// toHandlerWait949 is a celeris handler that waits up to hold on c.Context().
func toHandlerWait949(hold time.Duration, started chan<- struct{}, outcome chan<- toHandlerOutcome949) http.Handler {
	return celeris.ToHandler(func(c *celeris.Context) error {
		o := toHandlerOutcome949{started: time.Now()}
		started <- struct{}{}
		ctx := c.Context()
		select {
		case <-ctx.Done():
			o.ended, o.err = true, ctx.Err()
		case <-time.After(hold):
		}
		o.at = time.Now()
		outcome <- o
		return c.String(200, "done")
	})
}

// serveToHandler949 serves h on a loopback listener over HTTP/1 and, with h2c,
// over cleartext HTTP/2 as well, and returns a client for the chosen protocol.
func serveToHandler949(t *testing.T, h http.Handler, h2c bool) (*http.Client, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	if h2c {
		srv.Protocols = new(http.Protocols)
		srv.Protocols.SetHTTP1(true)
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	tr := &http.Transport{DisableKeepAlives: !h2c}
	if h2c {
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetUnencryptedHTTP2(true)
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, "http://" + ln.Addr().String() + "/"
}

func TestToHandlerClientDepartureCancelsContext949(t *testing.T) {
	for _, proto := range []string{"http1", "h2c"} {
		t.Run(proto, func(t *testing.T) {
			started := make(chan struct{}, 1)
			outcome := make(chan toHandlerOutcome949, 1)
			cl, url := serveToHandler949(t, toHandlerWait949(8*time.Second, started, outcome), proto == "h2c")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			errc := make(chan error, 1)
			go func() {
				resp, err := cl.Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				errc <- err
			}()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the handler never started")
			}
			at := time.Now()
			cancel() // the client gives up: closes the connection (HTTP/1) or resets the stream (h2c)
			select {
			case o := <-outcome:
				t.Logf("MEASURE proto=%s ended=%v err=%v latency=%s", proto, o.ended, o.err, o.at.Sub(at).Round(time.Millisecond))
				if !o.ended {
					t.Fatalf("%s: the client left and c.Context() was still not done after %s", proto, o.at.Sub(o.started))
				}
				if o.err == nil || o.err.Error() != "context canceled" {
					t.Fatalf("%s: c.Context().Err() = %v, want context canceled", proto, o.err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("%s: the handler never reported", proto)
			}
			<-errc
		})
	}
}

// A deadline above ToHandler (http.TimeoutHandler) cancels r.Context(); the
// celeris handler must see it end.
func TestToHandlerTimeoutHandlerCancelsContext949(t *testing.T) {
	started := make(chan struct{}, 1)
	outcome := make(chan toHandlerOutcome949, 1)
	h := http.TimeoutHandler(toHandlerWait949(8*time.Second, started, outcome), 200*time.Millisecond, "timeout")
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 from the TimeoutHandler", resp.StatusCode)
	}
	select {
	case o := <-outcome:
		if !o.ended {
			t.Fatalf("http.TimeoutHandler fired and c.Context() was still not done after %s", o.at.Sub(o.started))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never reported")
	}
}

// The guard against cancelling too much: a client that stays gets its answer
// and its handler's context never ended.
func TestToHandlerStayingClientKeepsContext949(t *testing.T) {
	for _, proto := range []string{"http1", "h2c"} {
		t.Run(proto, func(t *testing.T) {
			started := make(chan struct{}, 1)
			outcome := make(chan toHandlerOutcome949, 1)
			cl, url := serveToHandler949(t, toHandlerWait949(600*time.Millisecond, started, outcome), proto == "h2c")
			resp, err := cl.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d, want 200", resp.StatusCode)
			}
			o := <-outcome
			if o.ended {
				t.Fatalf("%s: the client stayed and c.Context() ended (%v) after %s", proto, o.err, o.at.Sub(o.started))
			}
			if d := o.at.Sub(o.started); d < 550*time.Millisecond {
				t.Fatalf("%s: the handler waited %s of 600ms: the test did not hold the context open", proto, d)
			}
		})
	}
}
