//go:build linux

package celeris_test

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/probe"
)

// One RFC 7540 section 3.2.1 rule for "Upgrade: h2c", on every engine
// (celeris#889, items 41 and 43).
//
// RFC 7540 3.2.1: a request that upgrades "MUST include exactly one
// HTTP2-Settings header field", and a server "MUST NOT upgrade the
// connection to HTTP/2 if this header field is not present or if more than
// one is present". The celeris rule, the same on std, epoll, io_uring and
// adaptive:
//
//   - exactly one HTTP2-Settings header whose value decodes (base64url, a
//     whole number of 6-byte SETTINGS entries, which may be none) upgrades:
//     101 and stream 1;
//   - a missing, repeated or undecodable header does not: the request is served
//     as the HTTP/1.1 request it also is (200 here), never with an error
//     status, and the connection stays HTTP/1.1;
//   - an EMPTY value is one header whose payload is an empty SETTINGS frame
//     (RFC 7540 6.5 allows one with no entries): it upgrades.
//
// Before this rule std answered 500 to a missing, repeated or undecodable
// header, the native engines answered 101 to a repeated one, and they served
// an empty one as HTTP/1.1 where std upgraded. This table is the oracle all
// four engines are held to, so they cannot drift apart again.

const (
	// settings7540 is a valid payload: 4 whole SETTINGS entries (24 bytes).
	settings7540 = "AAMAAABkAAQAoAAAAAIAAAAA"
)

func b64url7540(n int) string {
	return base64.RawURLEncoding.EncodeToString(make([]byte, n))
}

func TestH2CSettingsRuleEveryEngine7540(t *testing.T) {
	shapes := []struct {
		name  string
		lines []string // HTTP2-Settings header lines, verbatim
		conn  string   // the Connection value
		want  string   // "101" or "http1-200"
	}{
		// Upgrades: exactly one header that decodes.
		{"one-valid", []string{"HTTP2-Settings: " + settings7540}, "Upgrade, HTTP2-Settings", "101"},
		{"one-valid-lowercase-name", []string{"http2-settings: " + settings7540}, "Upgrade, HTTP2-Settings", "101"},
		{"one-valid-OWS", []string{"HTTP2-Settings:   " + settings7540 + " \t"}, "Upgrade, HTTP2-Settings", "101"},
		{"one-valid-no-entries-6B", []string{"HTTP2-Settings: " + b64url7540(6)}, "Upgrade, HTTP2-Settings", "101"},
		{"one-empty-value", []string{"HTTP2-Settings: "}, "Upgrade, HTTP2-Settings", "101"},
		{"one-empty-value-no-space", []string{"HTTP2-Settings:"}, "Upgrade, HTTP2-Settings", "101"},

		// Missing: MUST NOT upgrade.
		{"missing", nil, "Upgrade, HTTP2-Settings", "http1-200"},

		// Repeated: MUST NOT upgrade, whatever the values are.
		{"repeated-valid-valid", []string{"HTTP2-Settings: " + settings7540, "HTTP2-Settings: " + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"repeated-valid-empty", []string{"HTTP2-Settings: " + settings7540, "HTTP2-Settings: "}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"repeated-empty-valid", []string{"HTTP2-Settings: ", "HTTP2-Settings: " + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"repeated-empty-empty", []string{"HTTP2-Settings: ", "HTTP2-Settings: "}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"repeated-mixed-case", []string{"HTTP2-Settings: " + settings7540, "http2-settings: " + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"repeated-three", []string{"HTTP2-Settings: " + settings7540, "HTTP2-Settings: " + settings7540, "HTTP2-Settings: " + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},

		// Undecodable: not upgraded, served as HTTP/1.1.
		{"invalid-base64", []string{"HTTP2-Settings: !!!notbase64!!!"}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"invalid-standard-alphabet", []string{"HTTP2-Settings: AAMA+/ABkAAQAoAAAAAIAAAAA"}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"invalid-length-5B", []string{"HTTP2-Settings: " + b64url7540(5)}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"invalid-length-7B", []string{"HTTP2-Settings: " + b64url7540(7)}, "Upgrade, HTTP2-Settings", "http1-200"},
		// Only SP and HTAB around the value are whitespace; a Unicode space
		// (here U+00A0, as UTF-8) is part of it, and does not decode.
		{"invalid-trailing-NBSP", []string{"HTTP2-Settings: " + settings7540 + "\xc2\xa0"}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"invalid-leading-NEL", []string{"HTTP2-Settings: \xc2\x85" + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},
		{"invalid-comma-list", []string{"HTTP2-Settings: " + settings7540 + "," + settings7540}, "Upgrade, HTTP2-Settings", "http1-200"},

		// Controls: the rest of the section 3.2 test is unchanged.
		{"valid-but-connection-lacks-token", []string{"HTTP2-Settings: " + settings7540}, "Upgrade", "http1-200"},
	}

	for _, e := range engines761 {
		for _, sh := range shapes {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				if e.name == "io_uring" && probe.Probe().IOUringTier < engine.Base {
					t.Skip("io_uring is not available on this host (Docker VM or old kernel)")
				}
				addr := startServer964(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto})
				wire := "GET /ping HTTP/1.1\r\nHost: example.com\r\nConnection: " + sh.conn + "\r\nUpgrade: h2c\r\n" +
					strings.Join(sh.lines, "\r\n")
				if len(sh.lines) > 0 {
					wire += "\r\n"
				}
				wire += "\r\n"

				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.WriteString(c, wire); err != nil {
					t.Fatal(err)
				}
				br := bufio.NewReader(c)
				if sh.want == "101" {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("read: %v", err)
					}
					if !strings.HasPrefix(line, "HTTP/1.1 101 ") {
						t.Fatalf("answered %q, want 101 Switching Protocols", strings.TrimSpace(line))
					}
					// The upgrade is complete, not only announced: after the
					// client preface the upgrade request is answered on
					// stream 1 (a 101 followed by a broken HTTP/2 stack is
					// worse than no upgrade).
					readUpgradedStream1_7540(t, c, br)
					return
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					peek, _ := br.Peek(br.Buffered())
					t.Fatalf("read response: %v (peek %q)", err, peek)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 || string(body) != "ok" {
					t.Fatalf("answered %d %q, want 200 \"ok\" (served as HTTP/1.1, not upgraded, no error status)", resp.StatusCode, body)
				}
				// The connection stays HTTP/1.1: a second request is answered.
				if _, err := io.WriteString(c, plainRequestPing7540); err != nil {
					t.Fatalf("second write: %v", err)
				}
				resp2, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("second response: %v", err)
				}
				body2, _ := io.ReadAll(resp2.Body)
				_ = resp2.Body.Close()
				if resp2.StatusCode != 200 || string(body2) != "ok" {
					t.Fatalf("second request answered %d %q, want 200 \"ok\"", resp2.StatusCode, body2)
				}
			})
		}
	}
}

const plainRequestPing7540 = "GET /ping HTTP/1.1\r\nHost: example.com\r\n\r\n"

// readUpgradedStream1_7540 finishes the h2c handshake on c (after the 101 and
// the rest of the response headers have been read from br) and requires the
// answer to the upgrade request on stream 1: 200 with the handler's body.
func readUpgradedStream1_7540(t *testing.T, c net.Conn, br *bufio.Reader) {
	t.Helper()
	// The 101 head: the status line is read, the blank line ends it.
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read 101 head: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	if _, err := io.WriteString(c, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, br)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	dec := hpack.NewDecoder(4096, nil)
	var status string
	var body []byte
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("read frame (status %q, body %q): %v", status, body, err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.HeadersFrame:
			if f.StreamID != 1 {
				continue
			}
			fields, err := dec.DecodeFull(f.HeaderBlockFragment())
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, hf := range fields {
				if hf.Name == ":status" {
					status = hf.Value
				}
			}
			if f.StreamEnded() {
				t.Fatalf("stream 1 ended without a body (status %q)", status)
			}
		case *http2.DataFrame:
			if f.StreamID == 1 {
				body = append(body, f.Data()...)
				if f.StreamEnded() {
					if status != "200" || string(body) != "ok" {
						t.Fatalf("stream 1: status %q body %q, want 200 \"ok\"", status, body)
					}
					return
				}
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("stream 1 reset: %v", f.ErrCode)
		case *http2.GoAwayFrame:
			t.Fatalf("GOAWAY: %v", f.ErrCode)
		}
	}
}
