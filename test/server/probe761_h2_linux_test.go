//go:build linux

package celeris_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// TestProbe761H2 is a measurement, not a regression test: for each engine and
// route it asks golang.org/x/net's http2.Transport (h2c, prior knowledge) for
// one response of 1 MiB, 5 MiB and 64 MiB and logs how long the transfer took
// (celeris#809: on epoll, io_uring and adaptive a 64 MiB response took about
// 16 s to this client while std took 0.07 s and the net/http client 0.3-0.6 s).
// It never fails on a slow transfer: a client that gives up at its 20 s timeout
// with part of the body is a result to read, not an error. It fails only when
// the transfer is wrong, that is when the server answers something other than
// an HTTP/2 200 with exactly the body, or the connection breaks for a reason
// other than that timeout.
//
// Each cell logs two lines the readers of the series parse:
//
//	ENGINE requested=<engine asked for> running=<engine EngineInfo reports>
//	PROBE761 h2 engine=<e> route=<sync|async> body=<n> status=<s> proto=<p> got=<bytes> end=<complete|timeout(conn open)|error> elapsed=<seconds>s
//
// A cell whose running engine differs from the requested one (io_uring
// unavailable on the host, which the cell logs and passes without a transfer)
// did not measure that engine.
// The cell names are TestProbe761H2/<engine>/<route>/<size>. The client is
// http2.Transport, not net/http's own, because the net/http client is what
// TestLargeResponseIsDeliveredH2 already covers.
func TestProbe761H2(t *testing.T) {
	for _, e := range engines761 {
		for _, route := range []string{"sync", "async"} {
			for _, n := range []int{1 << 20, 5 << 20, 64 << 20} {
				t.Run(fmt.Sprintf("%s/%s/%d", e.name, route, n), func(t *testing.T) {
					probe761H2Cell(t, e.name, e.eng, route == "async", n)
				})
			}
		}
	}
}

func probe761H2Cell(t *testing.T, name string, eng celeris.EngineType, async bool, n int) {
	if name == "io_uring" && probe.Probe().IOUringTier < engine.Base {
		// Not a skip: the cell measured nothing and says so in the line the
		// readers look for, which then reads running != requested.
		t.Logf("ENGINE requested=%s running=unavailable (io_uring is not available on this host)", eng)
		return
	}
	body := make([]byte, n)
	s, addr := startPickedPing1104(t, celeris.Config{Engine: eng}, func(s *celeris.Server) {
		ping1104(s)
		r := s.GET("/big", func(c *celeris.Context) error {
			return c.Blob(http.StatusOK, "application/octet-stream", body)
		})
		if async {
			r.Async()
		}
	})
	running := "none"
	if info := s.EngineInfo(); info != nil {
		running = info.Type.String()
	}
	t.Logf("ENGINE requested=%s running=%s", eng, running)

	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		},
	}}
	defer cl.CloseIdleConnections()

	route := "sync"
	if async {
		route = "async"
	}
	t0 := time.Now()
	resp, err := cl.Get("http://" + addr + "/big")
	got, status, proto := int64(-1), 0, 0
	if err == nil {
		status, proto = resp.StatusCode, resp.ProtoMajor
		got, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	elapsed := time.Since(t0)
	end := probe761H2End(err)
	t.Logf("PROBE761 h2 engine=%s route=%s body=%d status=%d proto=%d got=%d end=%s elapsed=%.3fs",
		name, route, n, status, proto, got, end, elapsed.Seconds())

	switch {
	case err == nil && (status != http.StatusOK || proto != 2 || got != int64(n)):
		t.Errorf("%s/%s body %d: status %d proto %d, received %d bytes with no error", name, route, n, status, proto, got)
	case err != nil && !probe761H2Timeout(err):
		t.Errorf("%s/%s body %d: the transfer broke after %d bytes: %s", name, route, n, got, end)
	}
}

// probe761H2Timeout reports whether err is the client's own timeout, the one
// end of a transfer the probe records instead of failing on.
func probe761H2Timeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func probe761H2End(err error) string {
	switch {
	case err == nil:
		return "complete"
	case probe761H2Timeout(err):
		return "timeout(conn open)"
	}
	return strings.ReplaceAll(err.Error(), "\n", " ")
}
