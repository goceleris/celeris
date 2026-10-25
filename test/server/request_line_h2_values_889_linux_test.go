//go:build linux

package celeris_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// celeris#889, items 40 and 45, on every engine over a real connection:
//
//   - an HTTP/1 request whose method is not a token, or whose request-target
//     holds a control byte (NUL, CR, LF), is a 400, on the first request of a
//     connection and on a later one on a keep-alive connection (the native
//     engines detect the protocol from the first bytes of a connection only);
//   - an HTTP/2 request whose :path or any field value holds a NUL, CR or LF
//     is a stream error (RFC 9113 sections 8.1.1 and 8.2.1) and is not served.
//
// std is in the table to pin net/http's answer, not to assume it.

func skipNoIOUring889(t *testing.T, name string) {
	t.Helper()
	if name == "io_uring" && probe.Probe().IOUringTier < engine.Base {
		t.Skip("io_uring is not available on this host (Docker VM or old kernel)")
	}
}

func TestInvalidRequestLineIs400EveryEngine889(t *testing.T) {
	const plain = "GET /ping HTTP/1.1\r\nHost: example.com\r\n\r\n"
	cases := []struct {
		name string
		line string
	}{
		{"method has NUL", "G\x00T /ping HTTP/1.1"},
		{"method ends in bare CR", "GET\r /ping HTTP/1.1"},
		{"method has a delimiter", "G(T /ping HTTP/1.1"},
		{"target has NUL", "GET /p\x00e=gzip HTTP/1.1"},
		{"target has bare CR", "GET /p\re HTTP/1.1"},
		{"target has bare LF", "GET /p\ne HTTP/1.1"},
		{"target has a control byte", "GET /p\x01e HTTP/1.1"},
	}
	for _, e := range engines761 {
		for _, tc := range cases {
			for _, pos := range []string{"first", "second"} {
				t.Run(e.name+"/"+pos+"/"+tc.name, func(t *testing.T) {
					skipNoIOUring889(t, e.name)
					addr := startServer964(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
					c, err := net.DialTimeout("tcp", addr, 5*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(10 * time.Second))
					br := bufio.NewReader(c)
					if pos == "second" {
						if _, err := io.WriteString(c, plain); err != nil {
							t.Fatal(err)
						}
						resp, err := http.ReadResponse(br, nil)
						if err != nil {
							t.Fatalf("first response: %v", err)
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode != 200 {
							t.Fatalf("first request answered %d", resp.StatusCode)
						}
					}
					if _, err := io.WriteString(c, tc.line+"\r\nHost: example.com\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						peek, _ := br.Peek(br.Buffered())
						t.Fatalf("response: %v (peek %q)", err, peek)
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 400 {
						t.Fatalf("answered %d %q, want 400", resp.StatusCode, body)
					}
				})
			}
		}
	}
}

// h2Probe889 sends the request on stream 1 of a prior-knowledge h2c
// connection and returns what came back for it: the response status (0 for
// none) and the RST_STREAM error code (-1 for none), read until the stream
// ends, is reset, the connection ends or 5 s pass.
func h2Probe889(t *testing.T, addr string, fields []hpack.HeaderField) (status int, rst int64) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, c)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var blk bytes.Buffer
	enc := hpack.NewEncoder(&blk)
	for _, f := range fields {
		if err := enc.WriteField(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: blk.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	dec := hpack.NewDecoder(4096, nil)
	rst = -1
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return status, rst
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			// No SETTINGS ACK is written: on a connection the server has
			// already closed it would draw a TCP RST that can discard the
			// RST_STREAM still queued for this client.
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			hs, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, h := range hs {
				if h.Name == ":status" {
					status = int(h.Value[0]-'0')*100 + int(h.Value[1]-'0')*10 + int(h.Value[2]-'0')
				}
			}
			if f.StreamEnded() {
				return status, rst
			}
		case *http2.DataFrame:
			if f.StreamID == 1 && f.StreamEnded() {
				return status, rst
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == 1 {
				return status, int64(f.ErrCode)
			}
		case *http2.GoAwayFrame:
			return status, rst
		}
	}
}

func TestH2NULCRLFInRequestIsRefusedEveryEngine889(t *testing.T) {
	base := func(path string, extra ...hpack.HeaderField) []hpack.HeaderField {
		return append([]hpack.HeaderField{
			{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
			{Name: ":path", Value: path}, {Name: ":authority", Value: "example.com"},
		}, extra...)
	}
	cases := []struct {
		name   string
		fields []hpack.HeaderField
	}{
		{":path has NUL", base("/ping\x00e=gzip")},
		{":path has LF", base("/ping\ne")},
		{":path has CR", base("/ping\re")},
		{"value has NUL", base("/ping", hpack.HeaderField{Name: "x-a", Value: "a\x00b"})},
		{"cookie has CR LF", base("/ping", hpack.HeaderField{Name: "cookie", Value: "a=b\r\nset-cookie: x=y"})},
	}
	for _, e := range engines761 {
		skipName := e.name
		t.Run(e.name+"/control-served", func(t *testing.T) {
			skipNoIOUring889(t, skipName)
			addr := startServer964(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
			if st, rst := h2Probe889(t, addr, base("/ping", hpack.HeaderField{Name: "x-a", Value: "a b%00"})); st != 200 || rst != -1 {
				t.Fatalf("a clean request: status %d, RST_STREAM %d, want 200 and none", st, rst)
			}
		})
		for _, tc := range cases {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				skipNoIOUring889(t, skipName)
				addr := startServer964(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
				st, rst := h2Probe889(t, addr, tc.fields)
				t.Logf("status %d, RST_STREAM %d", st, rst)
				if st != 0 || rst != int64(http2.ErrCodeProtocol) {
					t.Fatalf("status %d, RST_STREAM %d: want the stream reset with PROTOCOL_ERROR (%d) and not served", st, rst, http2.ErrCodeProtocol)
				}
			})
		}
	}
}
