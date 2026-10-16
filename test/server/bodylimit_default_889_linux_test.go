//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// Polish item 14 of celeris#889 (user decision, v1.6.0): the default of
// Config.MaxRequestBodySize is 4 MiB (it was 100 MB), on every engine and
// every way a request body reaches the handler. An explicit value and -1
// ("unlimited") keep working. celeris buffers the whole body in memory before
// the handler runs, so the default is what bounds a client's reach into the
// server's memory.
//
// The table drives, per engine (std, epoll, io_uring, adaptive) and per
// configuration (default, an explicit 8 MiB, -1, and an explicit 4 MiB as the
// control that was already true before the change), a body of exactly 4 MiB
// (accepted) and 4 MiB + 1 byte (refused unless the limit is higher) down
// each path that enforces the limit:
//
//	h1-cl     HTTP/1.1, Content-Length
//	h1-chunk  HTTP/1.1, Transfer-Encoding: chunked, 1 MiB chunks (celeris#1029)
//	h2        HTTP/2 prior knowledge, DATA frames, content-length declared
//	h2-nolen  HTTP/2 prior knowledge, DATA frames, no content-length
//	upgrade   HTTP/1.1 Upgrade: h2c request with a body (served as stream 1)
//	upgrade-chunk  the same with a chunked body
//
// Refusals are asserted exactly per path and engine: HTTP/1 and the Upgrade
// answer 413; HTTP/2 on the native engines resets the stream (RST_STREAM
// CANCEL), HTTP/2 on std goes through net/http, which answers 413.
//
// TestUpgradedConnectionLaterStreamBodyLimit889 adds the streams of a
// connection that was upgraded from HTTP/1.1: epoll and io_uring build that
// connection's HTTP/2 state in a second place.

const bodyMiB889 = 1 << 20

type bodyCfg889 struct {
	name  string
	limit int64 // Config.MaxRequestBodySize
	max   int64 // the effective limit: -1 unlimited
}

var bodyCfgs889 = []bodyCfg889{
	{"default", 0, 4 * bodyMiB889},
	{"explicit-8MiB", 8 * bodyMiB889, 8 * bodyMiB889},
	{"unlimited", -1, -1},
	{"explicit-4MiB", 4 * bodyMiB889, 4 * bodyMiB889},
}

// bodyServer889 starts a server on eng with cfg's limit, Protocol Auto (H1,
// prior-knowledge H2 and the h2c Upgrade), and a POST route that answers with
// the number of body bytes the handler saw.
func bodyServer889(t *testing.T, eng celeris.EngineType, limit int64) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := celeris.New(celeris.Config{Addr: addr, Engine: eng, Protocol: celeris.Auto, MaxRequestBodySize: limit})
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		s.POST("/up", func(c *celeris.Context) error { return c.String(http.StatusOK, "n=%d", len(c.Body())) })
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}

const bodyUpgradeHead889 = "POST /up HTTP/1.1\r\nHost: a\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n" +
	"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n"

// h1Body889 sends a POST (or an Upgrade: h2c POST when upgrade) with n body
// bytes, Content-Length or chunked, and returns the first response line and,
// for a 200, the body. For an upgrade it reads on: the 101, then stream 1's
// response over HTTP/2, and returns that stream's status and body in
// h2status and body.
func h1Body889(t *testing.T, addr string, n int64, chunked, upgrade bool) (first, h2status, body string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	head := "POST /up HTTP/1.1\r\nHost: a\r\n"
	if upgrade {
		head = bodyUpgradeHead889
	}
	if chunked {
		head += "Transfer-Encoding: chunked\r\n\r\n"
	} else {
		head += fmt.Sprintf("Content-Length: %d\r\n\r\n", n)
	}
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}
	// The body goes out from a goroutine: a server that refuses early stops
	// reading, and the write then fails with EPIPE or ECONNRESET, which is
	// not the test's concern; the answer is.
	go func() {
		chunk := bytes.Repeat([]byte{'x'}, bodyMiB889)
		for left := n; left > 0; {
			k := int64(len(chunk))
			if left < k {
				k = left
			}
			var err error
			if chunked {
				_, err = fmt.Fprintf(c, "%x\r\n", k)
				if err == nil {
					_, err = c.Write(chunk[:k])
				}
				if err == nil {
					_, err = io.WriteString(c, "\r\n")
				}
			} else {
				_, err = c.Write(chunk[:k])
			}
			if err != nil {
				return
			}
			left -= k
		}
		if chunked {
			_, _ = io.WriteString(c, "0\r\n\r\n")
		}
	}()
	br := bufio.NewReaderSize(c, 64<<10)
	line, err := br.ReadString('\n')
	first = strings.TrimSpace(line)
	if err != nil && first == "" {
		return "read error: " + err.Error(), "", ""
	}
	if upgrade {
		if !strings.HasPrefix(first, "HTTP/1.1 101") {
			return first, "", ""
		}
		for { // the rest of the 101 head
			l, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading the 101 head: %v", err)
			}
			if l == "\r\n" {
				break
			}
		}
		fr := http2.NewFramer(c, br)
		if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
			t.Fatal(err)
		}
		if err := fr.WriteSettings(); err != nil {
			t.Fatal(err)
		}
		h2status, body = readH2Response944(t, fr)
		return first, h2status, body
	}
	hdrs := map[string]string{}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return first, "", ""
		}
		if l == "\r\n" {
			break
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			hdrs[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	var cl int64
	if _, err := fmt.Sscanf(hdrs["content-length"], "%d", &cl); err != nil || cl > 64 {
		return first, "", ""
	}
	buf := make([]byte, cl)
	_, _ = io.ReadFull(br, buf)
	return first, "", string(buf)
}

// h2Client889 is an h2c (prior knowledge) client: net/http's own transport,
// HTTP/2 without TLS only.
func h2Client889() *http.Transport {
	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	return &http.Transport{Protocols: &protos}
}

// genBody889 yields n bytes without holding them.
type genBody889 struct{ left int64 }

func (g *genBody889) Read(p []byte) (int, error) {
	if g.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.left {
		p = p[:g.left]
	}
	for i := range p {
		p[i] = 'y'
	}
	g.left -= int64(len(p))
	return len(p), nil
}

// h2Body889 POSTs n bytes over HTTP/2 prior knowledge and returns the status
// and body, or the error the round trip ended with.
func h2Body889(t *testing.T, addr string, n int64, declare bool) (status int, body string, err error) {
	t.Helper()
	tr := h2Client889()
	defer tr.CloseIdleConnections()
	req, rerr := http.NewRequest(http.MethodPost, "http://"+addr+"/up", &genBody889{left: n})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if declare {
		req.ContentLength = n
	} else {
		req.ContentLength = -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := tr.RoundTrip(req.WithContext(ctx))
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode, string(b), nil
}

func TestDefaultMaxRequestBodySize4MiB889(t *testing.T) {
	for _, e := range engines761 {
		for _, cfg := range bodyCfgs889 {
			t.Run(e.name+"/"+cfg.name, func(t *testing.T) {
				addr := bodyServer889(t, e.eng, cfg.limit)
				for _, size := range []struct {
					name string
					n    int64
				}{{"4MiB", 4 * bodyMiB889}, {"4MiB+1", 4*bodyMiB889 + 1}} {
					ok := cfg.max < 0 || size.n <= cfg.max
					wantBody := fmt.Sprintf("n=%d", size.n)
					type h1Path struct {
						name             string
						chunked, upgrade bool
					}
					paths := []h1Path{{"h1-cl", false, false}, {"upgrade", false, true}, {"h1-chunk", true, false}, {"upgrade-chunk", true, true}}
					for _, path := range paths {
						t.Run(size.name+"/"+path.name, func(t *testing.T) {
							first, h2status, body := h1Body889(t, addr, size.n, path.chunked, path.upgrade)
							switch {
							case ok && !path.upgrade:
								if !strings.HasPrefix(first, "HTTP/1.1 200") || body != wantBody {
									t.Fatalf("limit %s, %d body bytes: got %q body %q, want 200 %q", cfg.name, size.n, first, body, wantBody)
								}
							case ok:
								if !strings.HasPrefix(first, "HTTP/1.1 101") || h2status != "200" || body != wantBody {
									t.Fatalf("limit %s, %d Upgrade body bytes: got %q, stream 1 status %q body %q, want 101, 200 %q", cfg.name, size.n, first, h2status, body, wantBody)
								}
							default:
								if !strings.HasPrefix(first, "HTTP/1.1 413") {
									t.Fatalf("limit %s, %d body bytes: got %q, want 413", cfg.name, size.n, first)
								}
							}
						})
					}
					for _, declare := range []bool{true, false} {
						name := "h2-nolen"
						if declare {
							name = "h2"
						}
						t.Run(size.name+"/"+name, func(t *testing.T) {
							status, body, err := h2Body889(t, addr, size.n, declare)
							switch {
							case ok:
								if err != nil || status != 200 || body != wantBody {
									t.Fatalf("limit %s, %d body bytes: status %d body %q err %v, want 200 %q", cfg.name, size.n, status, body, err, wantBody)
								}
							case e.name == "std":
								if err != nil || status != 413 {
									t.Fatalf("limit %s, %d body bytes: status %d err %v, want 413", cfg.name, size.n, status, err)
								}
							default:
								// net/http's HTTP/2 client reports the reset as its own
								// stream error type: "stream error: stream ID N; CANCEL; received from peer".
								if err == nil || !strings.Contains(err.Error(), "stream error") || !strings.Contains(err.Error(), "CANCEL") {
									t.Fatalf("limit %s, %d body bytes: status %d body %q err %v, want RST_STREAM CANCEL", cfg.name, size.n, status, body, err)
								}
							}
						})
					}
				}
			})
		}
	}
}

// h2Upgraded889 is a client of an h2c connection that was upgraded from
// HTTP/1.1 (a GET /ping with Upgrade: h2c, answered on stream 1). It opens
// stream 3 as a POST with n body bytes under the connection's flow control and
// reports how the server ended it: "status 200 n=..." or "rst CANCEL".
func h2Upgraded889(t *testing.T, addr string, n int64) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// readerDone is closed when the reader goroutine below has exited, so
	// that nothing of the helper (its t.Logf included) outlives the test.
	readerDone := make(chan struct{})
	started := false
	defer func() {
		_ = c.Close()
		if started {
			<-readerDone
		}
	}()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := io.WriteString(c, "GET /ping HTTP/1.1\r\nHost: a\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Fatalf("upgrade answered %q, %v", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the 101 head: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	fr := http2.NewFramer(c, br)
	fr.SetMaxReadFrameSize(1 << 20)
	var (
		wmu     sync.Mutex
		mu      sync.Mutex
		cond    = sync.NewCond(&mu)
		connWin = int64(65535)
		strWin  = int64(65535)
		maxFr   = 16384
		outcome string
		body    bytes.Buffer
		status  string
	)
	write := func(f func() error) {
		wmu.Lock()
		defer wmu.Unlock()
		if err := f(); err != nil {
			t.Logf("write: %v", err)
		}
	}
	write(func() error { _, err := io.WriteString(c, http2.ClientPreface); return err })
	write(func() error { return fr.WriteSettings() })
	started = true
	go func() {
		defer close(readerDone)
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
			if f.Name == ":status" {
				status = f.Value
			}
		})
		for {
			f, err := fr.ReadFrame()
			mu.Lock()
			if err != nil {
				if outcome == "" {
					outcome = "read error: " + err.Error()
				}
				cond.Broadcast()
				mu.Unlock()
				return
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = f.ForeachSetting(func(s http2.Setting) error {
						switch s.ID {
						case http2.SettingInitialWindowSize:
							strWin += int64(s.Val) - 65535
						case http2.SettingMaxFrameSize:
							maxFr = int(s.Val)
						}
						return nil
					})
					mu.Unlock()
					write(fr.WriteSettingsAck)
					mu.Lock()
				}
			case *http2.WindowUpdateFrame:
				switch f.StreamID {
				case 0:
					connWin += int64(f.Increment)
				case 3:
					strWin += int64(f.Increment)
				}
			case *http2.HeadersFrame:
				if f.StreamID == 3 {
					_, _ = dec.Write(f.HeaderBlockFragment())
					if f.StreamEnded() {
						outcome = "status " + status
					}
				}
			case *http2.DataFrame:
				if f.StreamID == 3 {
					body.Write(f.Data())
					if f.StreamEnded() {
						outcome = "status " + status + " " + body.String()
					}
				}
			case *http2.RSTStreamFrame:
				if f.StreamID == 3 {
					outcome = "rst " + f.ErrCode.String()
				}
			case *http2.GoAwayFrame:
				if outcome == "" {
					outcome = "goaway " + f.ErrCode.String()
				}
			}
			cond.Broadcast()
			mu.Unlock()
		}
	}()
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":authority", addr}, {":path", "/up"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	write(func() error {
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hb.Bytes(), EndHeaders: true})
	})
	chunk := bytes.Repeat([]byte{'z'}, 1<<20)
	for left := n; left > 0; {
		mu.Lock()
		for outcome == "" && (connWin <= 0 || strWin <= 0) {
			cond.Wait()
		}
		if outcome != "" {
			mu.Unlock()
			break
		}
		k := int64(maxFr)
		if k > left {
			k = left
		}
		if k > connWin {
			k = connWin
		}
		if k > strWin {
			k = strWin
		}
		connWin -= k
		strWin -= k
		mu.Unlock()
		last := k == left
		write(func() error { return fr.WriteData(3, last, chunk[:k]) })
		left -= k
	}
	mu.Lock()
	defer mu.Unlock()
	for outcome == "" {
		cond.Wait()
	}
	return outcome
}

// TestUpgradedConnectionLaterStreamBodyLimit889: the limit holds for the
// streams of a connection that was upgraded from HTTP/1.1, not only for the
// Upgrade request's own body (celeris RULE 61: epoll and io_uring build that
// connection's HTTP/2 state in a second place).
func TestUpgradedConnectionLaterStreamBodyLimit889(t *testing.T) {
	for _, e := range engines761 {
		for _, cfg := range bodyCfgs889 {
			t.Run(e.name+"/"+cfg.name, func(t *testing.T) {
				addr := bodyServer889(t, e.eng, cfg.limit)
				for _, n := range []int64{4 * bodyMiB889, 4*bodyMiB889 + 1} {
					got := h2Upgraded889(t, addr, n)
					ok := cfg.max < 0 || n <= cfg.max
					want := fmt.Sprintf("status 200 n=%d", n)
					if !ok {
						want = "rst CANCEL"
						if e.name == "std" {
							want = "status 413"
						}
					}
					if !strings.HasPrefix(got, want) {
						t.Fatalf("stream 3 of an upgraded connection, %d body bytes, limit %s: got %q, want %q", n, cfg.name, got, want)
					}
				}
			})
		}
	}
}

// TestExpectContinueOverLimit889: a request that declares a Content-Length
// over the limit and asks for "Expect: 100-continue" is refused with 413
// alone, never "100 Continue" first (the client would start sending a body the
// server has already decided to refuse). The head goes in one write (the fast
// path of the native engines) and split over two (their buffered path, also
// taken by the async-route re-run): both must agree. A declared length at the
// limit still gets its 100 Continue. The refusal carries the reason phrase
// "Request Entity Too Large" on every engine, as std's does.
func TestExpectContinueOverLimit889(t *testing.T) {
	const limit = 4 * bodyMiB889
	for _, e := range engines761 {
		addr := bodyServer889(t, e.eng, 0)
		for _, split := range []bool{false, true} {
			for _, n := range []int64{limit, limit + 1} {
				t.Run(fmt.Sprintf("%s/split=%v/n=%d", e.name, split, n), func(t *testing.T) {
					c, err := net.DialTimeout("tcp", addr, 5*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(2 * time.Second))
					a := "POST /up HTTP/1.1\r\nHost: a\r\n"
					b := fmt.Sprintf("Expect: 100-continue\r\nContent-Length: %d\r\n\r\n", n)
					if split {
						_, _ = io.WriteString(c, a)
						time.Sleep(100 * time.Millisecond)
						_, _ = io.WriteString(c, b)
					} else {
						_, _ = io.WriteString(c, a+b)
					}
					// Every status line the server sends until it closes or
					// the 2 s deadline passes (an accepted length waits for
					// a body that never comes).
					var statuses []string
					br := bufio.NewReader(c)
					for {
						l, err := br.ReadString('\n')
						if err != nil {
							break
						}
						if strings.HasPrefix(l, "HTTP/1.1 ") {
							statuses = append(statuses, strings.TrimSpace(l))
						}
					}
					want := []string{"HTTP/1.1 100 Continue"}
					if n > limit {
						want = []string{"HTTP/1.1 413 Request Entity Too Large"}
					}
					if fmt.Sprint(statuses) != fmt.Sprint(want) {
						t.Fatalf("status lines %q, want %q", statuses, want)
					}
				})
			}
		}
	}
}
