// Package benchcmp_sse benchmarks celeris's middleware/sse Broker against
// other widely used Go SSE libraries for the publish-to-N-subscribers
// fan-out path. Lives in a separate module so competitor dependencies
// stay isolated from the main celeris build.
//
// Run with: go test -bench . -benchmem ./test/benchcmp_sse/...
//
// The mage `benchcmpSSE` target (see top-level magefile.go) wraps this
// invocation with the framework's standard reporting + benchstat
// post-processing.
//
// Both setups use net/http transport + httptest servers so the
// comparison is apples-to-apples — neither library gets the advantage
// of a synthetic in-memory connection.
package benchcmp_sse

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gosse "github.com/tmaxmax/go-sse"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

type subscriber struct {
	resp *http.Response
	done chan struct{}
}

// attachSubscribers opens n SSE connections to url and starts a body
// drainer per connection so the server's writes never stall.
func attachSubscribers(b *testing.B, client *http.Client, url string, n int) ([]subscriber, func()) {
	b.Helper()
	subs := make([]subscriber, 0, n)
	for range n {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("subscribe: %v", err)
		}
		done := make(chan struct{})
		go func(r *http.Response) {
			defer close(done)
			_, _ = io.Copy(io.Discard, r.Body)
		}(resp)
		subs = append(subs, subscriber{resp: resp, done: done})
	}
	cleanup := func() {
		for _, s := range subs {
			_ = s.resp.Body.Close()
			<-s.done
		}
	}
	return subs, cleanup
}

func benchmarkCelerisBroker(b *testing.B, n int) {
	br := sse.NewBroker(sse.BrokerConfig{SubscriberBuffer: 1024})
	defer br.Close()

	srv := celeris.New(celeris.Config{Engine: celeris.Std})
	srv.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(c *sse.Client) {
			unsub := br.Subscribe(c)
			defer unsub()
			<-c.Context().Done()
		},
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.StartWithListener(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-srvDone
	}()

	url := "http://" + ln.Addr().String() + "/events"
	_, cleanup := attachSubscribers(b, http.DefaultClient, url, n)
	defer cleanup()
	for br.SubscriberCount() != n {
		time.Sleep(2 * time.Millisecond)
	}

	ev := sse.Event{Data: "hello"}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_ = br.Publish(ev)
	}
}

func benchmarkGoSSE(b *testing.B, n int) {
	server := &gosse.Server{}
	ts := httptest.NewServer(server)
	defer ts.Close()

	_, cleanup := attachSubscribers(b, ts.Client(), ts.URL, n)
	defer cleanup()
	time.Sleep(100 * time.Millisecond)

	msg := &gosse.Message{}
	msg.AppendData("hello")
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_ = server.Publish(msg)
	}
}

func BenchmarkCelerisBrokerPublishTo100(b *testing.B)  { benchmarkCelerisBroker(b, 100) }
func BenchmarkCelerisBrokerPublishTo1000(b *testing.B) { benchmarkCelerisBroker(b, 1000) }
func BenchmarkGoSSEPublishTo100(b *testing.B)          { benchmarkGoSSE(b, 100) }
func BenchmarkGoSSEPublishTo1000(b *testing.B)         { benchmarkGoSSE(b, 1000) }
