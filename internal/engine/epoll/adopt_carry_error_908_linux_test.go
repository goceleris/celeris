//go:build linux

package epoll

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#908: a transplant's carried bytes are replayed by the adopting
// engine in place of the recv that would have read them, so what the replay
// does with ProcessH1's verdict must be what the recv path does. These tests
// hand the adopting engine the carry through AdoptConn, the TransplantTarget
// API the adaptive switch uses, and read what the client gets back. They are
// deterministic: the bytes are the carry, the client sends nothing.

// hasEOF908 reports whether c reaches EOF (the server closed it) within d,
// reading through br.
func hasEOF908(c net.Conn, br *bufio.Reader, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := br.ReadByte()
	return err == io.EOF || errors.Is(err, syscall.ECONNRESET)
}

// adoptCarry908 adopts a fresh conn that carries the bytes in carried, and
// returns the client end with a reader on it.
func adoptCarry908(t *testing.T, adopt func(int, engine.Carryover) error, carried string) (net.Conn, *bufio.Reader) {
	t.Helper()
	client, fd := adoptable543(t)
	if err := adopt(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String(), Buffered: []byte(carried)}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn with a carry: %v", err)
	}
	return client, bufio.NewReader(client)
}

// onErr908Handler is carry543Handler plus one route, /onerr, whose handler
// registers a connection error callback, the way a WebSocket upgrade does
// (Stream.OnWSSetError becomes H1State.OnError): a conn that ends on an error
// must deliver it to that callback, then close.
type onErr908Handler struct {
	*carry543Handler
	errs chan error
}

// big908 is the size of the /big response: well over the socket buffers of a
// loopback pair, so the first flush cannot take it whole.
const big908 = 32 << 20

func (h *onErr908Handler) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.Path == "/big" && s.ResponseWriter != nil {
		body := bytes.Repeat([]byte{'b'}, big908)
		return s.ResponseWriter.WriteResponse(s, 200,
			[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
	}
	if s.Path == "/onerr" && s.OnWSSetError != nil {
		s.OnWSSetError(func(err error) {
			select {
			case h.errs <- err:
			default:
			}
		})
	}
	return h.carry543Handler.HandleStream(ctx, s)
}

type step908 struct {
	code int
	body string // "" for an error response, whose body is not asserted
}

// runCarryReplies908 adopts a conn carrying carried and requires the client
// to read exactly the responses in want, in order, and then EOF.
func runCarryReplies908(t *testing.T, adopt func(int, engine.Carryover) error, carried string, want []step908) {
	t.Helper()
	client, br := adoptCarry908(t, adopt, carried)
	for i, w := range want {
		code, body, err := readBody543(client, br, 3*time.Second)
		if err != nil {
			t.Fatalf("response %d of %d: no response read before EOF or timeout: %v (the carried bytes were answered with a silent close)", i+1, len(want), err)
		}
		if code != w.code || (w.body != "" && body != w.body) {
			t.Fatalf("response %d of %d: got %d %q, want %d %q", i+1, len(want), code, body, w.code, w.body)
		}
	}
	if !hasEOF908(client, br, 3*time.Second) {
		t.Errorf("the conn stayed open after the last response: the replay must close a conn whose request ended it")
	}
}

const (
	carryBad908   = "GET /bad HTTP/1.1\r\n\r\n" // HTTP/1.1 without Host: ProcessH1 writes a 400 and returns the parse error
	carryClose908 = "GET /close HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	carryGood908  = "GET /first HTTP/1.1\r\nHost: x\r\n\r\n"
)

func testCarryParseError908(t *testing.T, adopt func(int, engine.Carryover) error) {
	runCarryReplies908(t, adopt, carryBad908, []step908{{400, ""}})
}

func testCarryParseErrorAfterGood908(t *testing.T, adopt func(int, engine.Carryover) error) {
	runCarryReplies908(t, adopt, carryGood908+carryBad908, []step908{{200, "/first"}, {400, ""}})
}

func testCarryConnectionClose908(t *testing.T, adopt func(int, engine.Carryover) error) {
	runCarryReplies908(t, adopt, carryClose908, []step908{{200, "/close"}})
}

// h2cUpgradeHead908 is an h2c upgrade request for GET /u.
func h2cUpgradeHead908() string {
	settings := base64.RawURLEncoding.EncodeToString([]byte{0, 3, 0, 0, 0, 100, 0, 4, 0, 0, 255, 255})
	return "GET /u HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
		"HTTP2-Settings: " + settings + "\r\n\r\n"
}

// testCarryH2CUpgrade908 adopts a conn whose carry is an h2c upgrade request.
// The client must get the 101, then HTTP/2: the response to the upgrade
// request on stream 1, and a working second stream.
func testCarryH2CUpgrade908(t *testing.T, adopt func(int, engine.Carryover) error) {
	client, br := adoptCarry908(t, adopt, h2cUpgradeHead908())
	_ = client.SetDeadline(time.Now().Add(6 * time.Second))
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("the upgrade request got no 101: %v (the replay closed the conn)", err)
	}
	if !strings.Contains(status, " 101 ") {
		t.Fatalf("upgrade status %q, want 101", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read the 101 head: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	h2Streams908(t, client, br)
}

// h2Streams908 speaks HTTP/2 on a conn whose 101 has been read.
func h2Streams908(t *testing.T, client net.Conn, br *bufio.Reader) {
	t.Helper()
	// HTTP/2 from here: the client preface and SETTINGS, then a request on
	// stream 3. The framer reads what the server sends (its SETTINGS, the
	// answer to stream 1, the answer to stream 3).
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/s3"}, {":authority", "x"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	fr := http2.NewFramer(client, br)
	if _, err := client.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatalf("write the client preface: %v", err)
	}
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("write SETTINGS: %v", err)
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatalf("write HEADERS: %v", err)
	}
	bodies := map[uint32]string{}
	done := map[uint32]bool{}
	for !done[1] || !done[3] {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("read an h2 frame after the 101 (stream 1 done=%v, stream 3 done=%v): %v", done[1], done[3], err)
		}
		if d, ok := f.(*http2.DataFrame); ok {
			bodies[d.StreamID] += string(d.Data())
			if d.StreamEnded() {
				done[d.StreamID] = true
			}
		}
	}
	if bodies[1] != "/u" || bodies[3] != "/s3" {
		t.Errorf("h2 answers: stream 1 %q (want /u), stream 3 %q (want /s3)", bodies[1], bodies[3])
	}
}

// testCarryOnError908: the carried requests are one that registers an error
// callback and one that fails to parse. The client gets the 200 and the 400,
// and the callback gets the parse error.
func testCarryOnError908(t *testing.T, adopt func(int, engine.Carryover) error, h *onErr908Handler) {
	runCarryReplies908(t, adopt, "GET /onerr HTTP/1.1\r\nHost: x\r\n\r\n"+carryBad908, []step908{{200, "/onerr"}, {400, ""}})
	select {
	case err := <-h.errs:
		if err == nil {
			t.Errorf("the error callback got a nil error")
		}
	case <-time.After(2 * time.Second):
		t.Errorf("the conn ended on a parse error and its error callback was never called (the recv path calls it)")
	}
}

// testRecvOnError908 is the control of testCarryOnError908: the same bytes
// read off the socket by the engine itself (no adoption) produce the same
// three results, so the carried replay is held to what the recv path does.
func testRecvOnError908(t *testing.T, e *Engine, h *onErr908Handler) {
	client, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("GET /onerr HTTP/1.1\r\nHost: x\r\n\r\n" + carryBad908)); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(client)
	for i, want := range []int{200, 400} {
		code, _, err := readBody543(client, br, 3*time.Second)
		if err != nil || code != want {
			t.Fatalf("recv path response %d: got %d, err %v, want %d", i+1, code, err, want)
		}
	}
	if !hasEOF908(client, br, 3*time.Second) {
		t.Errorf("recv path: the conn stayed open after the 400")
	}
	select {
	case err := <-h.errs:
		if err == nil {
			t.Errorf("recv path: the error callback got a nil error")
		}
	case <-time.After(2 * time.Second):
		t.Errorf("recv path: the error callback was never called")
	}
}

// testCarryLargeResponse908 adopts a conn whose carried request has a response
// larger than the socket buffers, with the client reading only after the
// adoption. The replay's flush is partial; the rest must follow on EPOLLOUT
// (epoll) or the send completion (io_uring), as it does for a request the
// engine read itself, and the conn must go on serving.
func testCarryLargeResponse908(t *testing.T, adopt func(int, engine.Carryover) error) {
	client, br := adoptCarry908(t, adopt, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
	time.Sleep(200 * time.Millisecond) // the replay has flushed what the kernel took
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the /big response head: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != big908 {
		t.Fatalf("read %d of %d body bytes of /big: %v (the replay's partial flush left the rest queued with nothing to send it)", n, big908, err)
	}
	_ = resp.Body.Close()
	if _, err := client.Write([]byte("GET /after HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write the follow-up: %v", err)
	}
	if code, body, err := readBody543(client, br, 3*time.Second); err != nil || code != 200 || body != "/after" {
		t.Errorf("follow-up after /big: %d %q, %v", code, body, err)
	}
}

// testRecvLargeResponse908 is the control of testCarryLargeResponse908: the
// same request read off the socket by the engine itself, the client reading
// only after the engine has flushed what the kernel took.
func testRecvLargeResponse908(t *testing.T, e *Engine) {
	client, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	br := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the /big response head: %v", err)
	}
	if n, err := io.Copy(io.Discard, resp.Body); err != nil || n != big908 {
		t.Fatalf("recv path: read %d of %d body bytes of /big: %v", n, big908, err)
	}
}

// testCarryLargeClose908: the carried request answers Connection: close with a
// response larger than the socket buffers. The close must wait until the whole
// body has reached the kernel (closeWhenFlushed on epoll, the deferred close
// on io_uring): the client gets every byte, then EOF.
func testCarryLargeClose908(t *testing.T, adopt func(int, engine.Carryover) error) {
	client, br := adoptCarry908(t, adopt, "GET /big HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	time.Sleep(200 * time.Millisecond)
	_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the /big response head: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != big908 {
		t.Fatalf("read %d of %d body bytes of /big: %v (the conn was closed before its response had gone out)", n, big908, err)
	}
	_ = resp.Body.Close()
	if !hasEOF908(client, br, 3*time.Second) {
		t.Errorf("the conn stayed open after a Connection: close response")
	}
}

// start908 starts an epoll engine on a free loopback port and waits until it
// has loops to adopt onto. upgrade turns EnableH2Upgrade on.
func start908(t *testing.T, async, upgrade bool) (*Engine, *onErr908Handler) {
	h := &onErr908Handler{carry543Handler: newCarry543Handler(), errs: make(chan error, 4)}
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr: addr, Protocol: engine.HTTP1, AsyncHandlers: async, EnableH2Upgrade: upgrade,
		Resources: resource.Resources{Workers: 2},
	}, h)
	if err != nil {
		t.Fatalf("epoll.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			return e, h
		}
		select {
		case lerr := <-done:
			t.Fatalf("epoll engine did not start: %v", lerr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("epoll engine did not start within 20s")
	return nil, nil
}

func TestEpollAdoptCarriedParseErrorSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testCarryParseError908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedParseErrorAfterGoodSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testCarryParseErrorAfterGood908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedConnectionCloseSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testCarryConnectionClose908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedH2CUpgradeSync908(t *testing.T) {
	e, _ := start908(t, false, true)
	testCarryH2CUpgrade908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedOnErrorSync908(t *testing.T) {
	e, h := start908(t, false, false)
	testCarryOnError908(t, e.AdoptConn, h)
}

func TestEpollAdoptCarriedLargeResponseSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testCarryLargeResponse908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedLargeCloseSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testCarryLargeClose908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedParseErrorAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testCarryParseError908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedParseErrorAfterGoodAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testCarryParseErrorAfterGood908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedConnectionCloseAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testCarryConnectionClose908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedH2CUpgradeAsync908(t *testing.T) {
	e, _ := start908(t, true, true)
	testCarryH2CUpgrade908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedOnErrorAsync908(t *testing.T) {
	e, h := start908(t, true, false)
	testCarryOnError908(t, e.AdoptConn, h)
}

func TestEpollAdoptCarriedLargeResponseAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testCarryLargeResponse908(t, e.AdoptConn)
}

func TestEpollAdoptCarriedLargeCloseAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testCarryLargeClose908(t, e.AdoptConn)
}

func TestEpollRecvPathOnErrorSync908(t *testing.T) {
	e, h := start908(t, false, false)
	testRecvOnError908(t, e, h)
}

func TestEpollRecvPathOnErrorAsync908(t *testing.T) {
	e, h := start908(t, true, false)
	testRecvOnError908(t, e, h)
}

func TestEpollRecvPathLargeResponseSync908(t *testing.T) {
	e, _ := start908(t, false, false)
	testRecvLargeResponse908(t, e)
}

func TestEpollRecvPathLargeResponseAsync908(t *testing.T) {
	e, _ := start908(t, true, false)
	testRecvLargeResponse908(t, e)
}
