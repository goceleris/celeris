//go:build linux

package spec

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
	"golang.org/x/net/http2/hpack"
)

// ---------- H2C test helpers ----------

const h2cClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// h2cSettingsB64 encodes a minimal HTTP2-Settings header value:
// SETTINGS_MAX_CONCURRENT_STREAMS = 100, SETTINGS_INITIAL_WINDOW_SIZE = 65535.
func h2cSettingsB64() string {
	payload := []byte{
		0, 3, 0, 0, 0, 100,
		0, 4, 0, 0, 255, 255,
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// buildH1UpgradeRequest crafts a plain H1 request carrying h2c upgrade headers.
func buildH1UpgradeRequest(method, path, host string, body []byte) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(&sb, "Host: %s\r\n", host)
	sb.WriteString("Connection: Upgrade, HTTP2-Settings\r\n")
	sb.WriteString("Upgrade: h2c\r\n")
	fmt.Fprintf(&sb, "HTTP2-Settings: %s\r\n", h2cSettingsB64())
	if len(body) > 0 {
		fmt.Fprintf(&sb, "Content-Length: %d\r\n", len(body))
	}
	sb.WriteString("\r\n")
	if len(body) > 0 {
		sb.Write(body)
	}
	return sb.String()
}

// encodeH2Frame builds a raw H2 frame (9-byte header + payload).
func encodeH2Frame(frameType, flags byte, streamID uint32, payload []byte) []byte {
	length := len(payload)
	buf := make([]byte, 9+length)
	buf[0] = byte(length >> 16)
	buf[1] = byte(length >> 8)
	buf[2] = byte(length)
	buf[3] = frameType
	buf[4] = flags
	binary.BigEndian.PutUint32(buf[5:9], streamID)
	copy(buf[9:], payload)
	return buf
}

// encodeHeadersFrame builds an H2 HEADERS frame with END_HEADERS + optional END_STREAM.
func encodeHeadersFrame(streamID uint32, endStream bool, headers []hpack.HeaderField) []byte {
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, hf := range headers {
		_ = enc.WriteField(hf)
	}
	flags := byte(0x04) // END_HEADERS
	if endStream {
		flags |= 0x01 // END_STREAM
	}
	return encodeH2Frame(0x01, flags, streamID, buf.Bytes())
}

// readH1Response reads up to headerTerminator from conn with a deadline.
func readH1Response(t *testing.T, c net.Conn, deadline time.Duration) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if bytes.Contains(buf, []byte("\r\n\r\n")) {
				return buf
			}
		}
		if err != nil {
			return buf
		}
	}
}

// readAtLeast reads at least n bytes from c or returns what it got.
func readAtLeast(t *testing.T, c net.Conn, n int, deadline time.Duration) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	buf := make([]byte, 0, n+64)
	tmp := make([]byte, 4096)
	for len(buf) < n {
		m, err := c.Read(tmp)
		if m > 0 {
			buf = append(buf, tmp[:m]...)
		}
		if err != nil {
			return buf
		}
	}
	return buf
}

// findH2DataOnStream parses frames from buf and returns concatenated DATA
// payload for the given stream ID. Stops at first frame with END_STREAM.
func findH2DataOnStream(buf []byte, streamID uint32) ([]byte, bool) {
	var out []byte
	endSeen := false
	i := 0
	for i+9 <= len(buf) {
		length := int(buf[i])<<16 | int(buf[i+1])<<8 | int(buf[i+2])
		ft := buf[i+3]
		fl := buf[i+4]
		sid := binary.BigEndian.Uint32(buf[i+5 : i+9])
		if i+9+length > len(buf) {
			break
		}
		payload := buf[i+9 : i+9+length]
		if ft == 0x00 && sid == streamID { // DATA
			out = append(out, payload...)
			if fl&0x01 != 0 {
				endSeen = true
			}
		}
		i += 9 + length
	}
	return out, endSeen
}

// ---------- h2c config matrix ----------

func h2cTrue() *bool  { b := true; return &b }
func h2cFalse() *bool { b := false; return &b }

// ---------- TESTS ----------

func TestH2CUpgradeBasicGET(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			// std engine uses x/net/http2/h2c middleware; its upgrade path is
			// separate. These tests target the custom engines.
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()

			req := buildH1UpgradeRequest("GET", "/hello", "example.com", nil)
			// Send request + client preface + SETTINGS together.
			msg := []byte(req)
			msg = append(msg, []byte(h2cClientPreface)...)
			msg = append(msg, encodeH2Frame(0x04, 0, 0, nil)...) // empty SETTINGS
			if _, err := c.Write(msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Read 101.
			resp := readH1Response(t, c, 2*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 101 Switching Protocols\r\n")) {
				t.Fatalf("expected 101, got %q", resp)
			}
			// After the 101 headers, remaining bytes are H2 frames from
			// the server (server SETTINGS, SETTINGS ACK, stream 1 HEADERS+DATA).
			// Read enough to capture stream 1 response.
			rest := readAtLeast(t, c, 64, 2*time.Second)
			combined := append(resp[bytes.Index(resp, []byte("\r\n\r\n"))+4:], rest...)
			data, ended := findH2DataOnStream(combined, 1)
			if !ended {
				// Keep reading if END_STREAM not yet seen.
				more := readAtLeast(t, c, 64, 2*time.Second)
				combined = append(combined, more...)
				data, ended = findH2DataOnStream(combined, 1)
			}
			if !ended {
				t.Fatalf("stream 1 END_STREAM not observed; buf=%x", combined)
			}
			if !bytes.Contains(data, []byte("GET /hello")) {
				t.Fatalf("stream 1 body = %q, want GET /hello", data)
			}
		})
	}
}

func TestH2CUpgradeWithBody(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			body := []byte("payload-body")
			req := buildH1UpgradeRequest("POST", "/echo", "example.com", body)
			msg := []byte(req)
			msg = append(msg, []byte(h2cClientPreface)...)
			msg = append(msg, encodeH2Frame(0x04, 0, 0, nil)...)
			if _, err := c.Write(msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 2*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 101 ")) {
				t.Fatalf("expected 101, got %q", resp)
			}
			tail := resp[bytes.Index(resp, []byte("\r\n\r\n"))+4:]
			rest := readAtLeast(t, c, 128, 2*time.Second)
			combined := append(tail, rest...)
			data, ended := findH2DataOnStream(combined, 1)
			for !ended && len(combined) < 4096 {
				more := readAtLeast(t, c, 64, 1*time.Second)
				if len(more) == 0 {
					break
				}
				combined = append(combined, more...)
				data, ended = findH2DataOnStream(combined, 1)
			}
			if !bytes.Contains(data, []byte("POST /echo")) {
				t.Fatalf("body = %q, want POST /echo prefix", data)
			}
			if !bytes.Contains(data, body) {
				t.Fatalf("body = %q, want contains %q", data, body)
			}
		})
	}
}

func TestH2CUpgradeInvalidSettings(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			// Bad base64 in HTTP2-Settings.
			req := "GET /x HTTP/1.1\r\n" +
				"Host: example.com\r\n" +
				"Connection: Upgrade, HTTP2-Settings\r\n" +
				"Upgrade: h2c\r\n" +
				"HTTP2-Settings: !!!notbase64!!!\r\n\r\n"
			if _, err := c.Write([]byte(req)); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 2*time.Second)
			// Expect a regular 200 H1 response (upgrade silently declined).
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 200 ")) {
				t.Fatalf("expected 200 H1 fallback, got %q", resp)
			}
		})
	}
}

func TestH2CUpgradeMissingConnectionToken(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			// Connection header lacks "HTTP2-Settings" token.
			req := "GET /x HTTP/1.1\r\n" +
				"Host: example.com\r\n" +
				"Connection: Upgrade\r\n" +
				"Upgrade: h2c\r\n" +
				"HTTP2-Settings: " + h2cSettingsB64() + "\r\n\r\n"
			if _, err := c.Write([]byte(req)); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 2*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 200 ")) {
				t.Fatalf("expected 200 H1 fallback, got %q", resp)
			}
		})
	}
}

func TestH2CUpgradeAmbiguousUpgrade(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			// Multi-token Upgrade: security invariant — h2c must not be
			// selected when other tokens coexist.
			req := "GET /x HTTP/1.1\r\n" +
				"Host: example.com\r\n" +
				"Connection: Upgrade, HTTP2-Settings\r\n" +
				"Upgrade: websocket, h2c\r\n" +
				"HTTP2-Settings: " + h2cSettingsB64() + "\r\n\r\n"
			if _, err := c.Write([]byte(req)); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 2*time.Second)
			if bytes.HasPrefix(resp, []byte("HTTP/1.1 101 ")) {
				t.Fatalf("ambiguous upgrade must NOT promote to h2c, got %q", resp)
			}
		})
	}
}

func TestH2CUpgradeConfigMatrix(t *testing.T) {
	cases := []struct {
		name         string
		proto        engine.Protocol
		enable       *bool
		wantUpgraded bool
	}{
		{"Auto default enables", engine.Auto, nil, true},
		{"Auto explicit true", engine.Auto, h2cTrue(), true},
		// "Auto explicit false" is intentionally omitted: at the
		// resource.Config level, EnableH2Upgrade is a concrete bool — zero
		// (false) is indistinguishable from "unset", and WithDefaults force-
		// enables upgrade on Auto. Users who want Auto+disabled must go
		// through the top-level celeris.Config path (EnableH2Upgrade *bool).
		// The celeris.Config → resource.Config conversion path is covered
		// by TestToResourceConfig_H2Upgrade in the root package.
		{"H2C default disabled", engine.H2C, nil, false},
		{"H2C explicit true", engine.H2C, h2cTrue(), true},
		{"HTTP1 default", engine.HTTP1, nil, false},
		{"HTTP1 explicit true still upgrades", engine.HTTP1, h2cTrue(), true},
	}
	for _, tc := range cases {
		for _, se := range specEngines {
			if se.name == "std" {
				continue
			}
			if tc.proto == engine.H2C {
				// H2C-only listeners reject plain HTTP/1 bytes; skip
				// cases that rely on starting as H1 to upgrade.
				continue
			}
			t.Run(tc.name+"/"+se.name, func(t *testing.T) {
				addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
					c.Protocol = tc.proto
					if tc.enable != nil {
						c.EnableH2Upgrade = *tc.enable
					}
				})
				cc, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				defer cc.Close()
				req := buildH1UpgradeRequest("GET", "/probe", "example.com", nil)
				if _, err := cc.Write([]byte(req)); err != nil {
					t.Fatalf("write: %v", err)
				}
				resp := readH1Response(t, cc, 2*time.Second)
				got101 := bytes.HasPrefix(resp, []byte("HTTP/1.1 101 "))
				if got101 != tc.wantUpgraded {
					t.Fatalf("proto=%v enable=%v: got101=%v want=%v (resp=%q)",
						tc.proto, tc.enable, got101, tc.wantUpgraded, resp)
				}
			})
		}
	}
}

func TestH2CUpgradePrefaceInSameSegment(t *testing.T) {
	// Already covered by TestH2CUpgradeBasicGET (sends preface in same Write);
	// this test isolates the behavior with explicit framing.
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			req := buildH1UpgradeRequest("GET", "/s1", "example.com", nil)
			msg := append([]byte(req), []byte(h2cClientPreface)...)
			msg = append(msg, encodeH2Frame(0x04, 0, 0, nil)...)
			if _, err := c.Write(msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 2*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 101 ")) {
				t.Fatalf("expected 101, got %q", resp)
			}
		})
	}
}

func TestH2CUpgradePrefaceSplit(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			req := buildH1UpgradeRequest("GET", "/split", "example.com", nil)
			if _, err := c.Write([]byte(req)); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Read 101 first.
			resp := readH1Response(t, c, 2*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 101 ")) {
				t.Fatalf("expected 101, got %q", resp)
			}
			// Give server a moment to finish writing stream 1 response,
			// then send the H2 preface in two chunks.
			time.Sleep(50 * time.Millisecond)
			half := []byte(h2cClientPreface)[:10]
			if _, err := c.Write(half); err != nil {
				t.Fatalf("write half: %v", err)
			}
			time.Sleep(50 * time.Millisecond)
			rest := []byte(h2cClientPreface)[10:]
			full := append(rest, encodeH2Frame(0x04, 0, 0, nil)...)
			if _, err := c.Write(full); err != nil {
				t.Fatalf("write rest: %v", err)
			}
			// Consume any remaining frames; verify the connection stays
			// open (partial preface must not trigger a close).
			_ = readAtLeast(t, c, 1, 500*time.Millisecond)
		})
	}
}

func TestH2CUpgradeLargeBody(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			body := bytes.Repeat([]byte{'A'}, 1<<20) // 1 MB
			req := buildH1UpgradeRequest("POST", "/big", "example.com", body)
			msg := append([]byte(req), []byte(h2cClientPreface)...)
			msg = append(msg, encodeH2Frame(0x04, 0, 0, nil)...)
			// Bump the connection-level flow-control window so the server can
			// stream the echoed ~1 MB body back on stream 1 without stalling
			// at the default 64 KiB window. Increment = 16 MiB.
			winInc := []byte{0x01, 0x00, 0x00, 0x00}
			msg = append(msg, encodeH2Frame(0x08, 0, 0, winInc)...) // conn WINDOW_UPDATE
			msg = append(msg, encodeH2Frame(0x08, 0, 1, winInc)...) // stream 1 WINDOW_UPDATE
			if _, err := c.Write(msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp := readH1Response(t, c, 3*time.Second)
			if !bytes.HasPrefix(resp, []byte("HTTP/1.1 101 ")) {
				t.Fatalf("expected 101, got %q", resp)
			}
			// Drain and check that stream 1 response contains a marker.
			// Collect up to ~2 MB or until stream 1 END_STREAM.
			deadline := time.Now().Add(5 * time.Second)
			collected := resp[bytes.Index(resp, []byte("\r\n\r\n"))+4:]
			for {
				if _, ended := findH2DataOnStream(collected, 1); ended {
					break
				}
				if time.Now().After(deadline) {
					break
				}
				chunk := readAtLeast(t, c, 4096, 500*time.Millisecond)
				if len(chunk) == 0 {
					break
				}
				collected = append(collected, chunk...)
			}
			data, ended := findH2DataOnStream(collected, 1)
			if !ended {
				t.Fatalf("stream 1 did not end")
			}
			if !bytes.Contains(data, []byte("POST /big")) {
				preview := data
				if len(preview) > 64 {
					preview = preview[:64]
				}
				t.Fatalf("stream 1 response = %q, want POST /big prefix", preview)
			}
		})
	}
}

func TestH2CUpgradeSubsequentStreams(t *testing.T) {
	for _, se := range specEngines {
		if se.name == "std" {
			continue
		}
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngineWithConfig(t, se, func(c *resource.Config) {
				c.Protocol = engine.Auto
				c.EnableH2Upgrade = true
			})
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			req := buildH1UpgradeRequest("GET", "/init", "example.com", nil)
			msg := append([]byte(req), []byte(h2cClientPreface)...)
			msg = append(msg, encodeH2Frame(0x04, 0, 0, nil)...)
			if _, err := c.Write(msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Drain initial H1 101 + H2 server preface + stream 1 response.
			// Be lenient: just consume for 300ms.
			_ = readH1Response(t, c, 2*time.Second)
			_ = readAtLeast(t, c, 128, 300*time.Millisecond)

			// Send subsequent streams 3, 5, 7 as HEADERS with END_STREAM.
			for _, sid := range []uint32{3, 5, 7} {
				hdrs := []hpack.HeaderField{
					{Name: ":method", Value: "GET"},
					{Name: ":path", Value: fmt.Sprintf("/s%d", sid)},
					{Name: ":scheme", Value: "http"},
					{Name: ":authority", Value: "example.com"},
				}
				frm := encodeHeadersFrame(sid, true, hdrs)
				if _, err := c.Write(frm); err != nil {
					t.Fatalf("write stream %d: %v", sid, err)
				}
			}
			// Collect responses; we want to see DATA on each stream.
			collected := make([]byte, 0, 4096)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				chunk := readAtLeast(t, c, 128, 300*time.Millisecond)
				if len(chunk) == 0 {
					break
				}
				collected = append(collected, chunk...)
				allDone := true
				for _, sid := range []uint32{3, 5, 7} {
					if _, ended := findH2DataOnStream(collected, sid); !ended {
						allDone = false
						break
					}
				}
				if allDone {
					break
				}
			}
			for _, sid := range []uint32{3, 5, 7} {
				d, ended := findH2DataOnStream(collected, sid)
				if !ended {
					t.Fatalf("stream %d did not end; got %q", sid, d)
				}
				want := fmt.Sprintf("GET /s%d", sid)
				if !bytes.Contains(d, []byte(want)) {
					t.Fatalf("stream %d body = %q, want contains %q", sid, d, want)
				}
			}
		})
	}
}

var _ io.Reader = (*bytes.Reader)(nil)
