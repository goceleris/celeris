//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 28. drainRead's async gate reads cs.protocol on the loop
// thread, while the conn's dispatch goroutine writes it (switchToH2Local, on an
// h2c upgrade it parses). A client that waits for the 101 before it sends
// anything else is ordered after the write by the 101's flush (x/sys's
// unix.Write and unix.Read are race-annotated as one synchronisation, which is
// all that keeps the detector quiet for it). A client that sends while the
// goroutine is still in the switch is not: this test holds the goroutine inside
// the switch (the handler of an HTTP/2 stream that came in the same write runs
// inline after the protocol is written) and sends a frame meanwhile. Run it
// under -race.

type edge885Handler struct {
	started chan struct{}
	release chan struct{}
}

func (h *edge885Handler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	if path == "/hold" {
		select {
		case h.started <- struct{}{}:
		default:
		}
		<-h.release
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(path))}}, []byte(path))
}

// Only /a is async: it promotes the conn, and the upgrade request behind it is
// parsed by the dispatch goroutine.
func (*edge885Handler) RouteAsync(_, path string) bool { return path == "/a" }
func (*edge885Handler) HasAsyncRoutes() bool           { return true }

func TestAsyncGateProtocolReadHasAnEdgeWithTheUpgradeSwitch885(t *testing.T) {
	h := &edge885Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	_, addr := startE2E885(t, true, h, func(c *resource.Config) { c.EnableH2Upgrade = true })
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/hold"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	var frames bytes.Buffer
	fr := http2.NewFramer(&frames, nil)
	_ = fr.WriteSettings()
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	// One write: a request that makes the conn async, the h2c upgrade request
	// behind it, and the client preface, SETTINGS and a request on stream 3.
	all := "GET /a HTTP/1.1\r\nHost: x\r\n\r\n" + h2cUpgradeHead908() + http2.ClientPreface + frames.String()
	if _, err := c.Write([]byte(all)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-h.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the goroutine never reached the HTTP/2 stream's handler after the switch")
	}
	// The goroutine is inside the switch, the protocol written. The client
	// sends a PING: the loop reads it and reads cs.protocol.
	var ping bytes.Buffer
	_ = http2.NewFramer(&ping, nil).WritePing(false, [8]byte{1})
	if _, err := c.Write(ping.Bytes()); err != nil {
		t.Fatalf("write the PING: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	close(h.release)

	// The conn goes on: the response to /a and the 101 arrive.
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var got strings.Builder
	for {
		line, err := br.ReadString('\n')
		got.WriteString(line)
		if err != nil {
			t.Fatalf("read the response to /a and the 101: %v (got %q)", err, got.String())
		}
		if strings.Contains(line, "HTTP/1.1 101") {
			return
		}
	}
}
