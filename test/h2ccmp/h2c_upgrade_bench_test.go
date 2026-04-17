package h2ccmp

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

const (
	h2cPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	frameData    byte = 0x0
	frameHeaders byte = 0x1
	frameSettings byte = 0x4
	frameGoAway  byte = 0x7

	flagEndStream  byte = 0x1
	flagAck        byte = 0x1
	flagEndHeaders byte = 0x4
)

// h2cSettingsB64 is the HTTP2-Settings header value used during upgrade:
// MAX_CONCURRENT_STREAMS=100, INITIAL_WINDOW_SIZE=65535.
func h2cSettingsB64() string {
	p := []byte{
		0, 3, 0, 0, 0, 100,
		0, 4, 0, 0, 255, 255,
	}
	return base64.RawURLEncoding.EncodeToString(p)
}

// buildUpgradeRequest builds a plain HTTP/1.1 GET with the RFC 7540 §3.2
// upgrade headers.
func buildUpgradeRequest(path, host string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Connection: Upgrade, HTTP2-Settings\r\n")
	b.WriteString("Upgrade: h2c\r\n")
	fmt.Fprintf(&b, "HTTP2-Settings: %s\r\n", h2cSettingsB64())
	b.WriteString("\r\n")
	return []byte(b.String())
}

// encodeFrame serializes a generic H2 frame (9-byte header + payload).
func encodeFrame(ftype, flags byte, streamID uint32, payload []byte) []byte {
	n := len(payload)
	f := make([]byte, 9+n)
	f[0] = byte(n >> 16)
	f[1] = byte(n >> 8)
	f[2] = byte(n)
	f[3] = ftype
	f[4] = flags
	binary.BigEndian.PutUint32(f[5:9], streamID)
	copy(f[9:], payload)
	return f
}

// encodeHeadersFrame builds a HEADERS frame with END_HEADERS + END_STREAM
// carrying :method :scheme :path :authority for a GET request.
func encodeHeadersFrame(streamID uint32, method, path, authority string) []byte {
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	_ = enc.WriteField(hpack.HeaderField{Name: ":method", Value: method})
	_ = enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "http"})
	_ = enc.WriteField(hpack.HeaderField{Name: ":path", Value: path})
	_ = enc.WriteField(hpack.HeaderField{Name: ":authority", Value: authority})
	return encodeFrame(frameHeaders, flagEndHeaders|flagEndStream, streamID, buf.Bytes())
}

// readAll reads at least min bytes or until deadline.
func readAtLeast(c net.Conn, out []byte, minBytes int, deadline time.Duration) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	tmp := make([]byte, 4096)
	for len(out) < minBytes {
		n, err := c.Read(tmp)
		if n > 0 {
			out = append(out, tmp[:n]...)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// consumeH1_101 reads until "\r\n\r\n" on the response, returns the trailing bytes
// (everything after the blank line).
func consumeH1_101(c net.Conn, deadline time.Duration) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	buf := make([]byte, 0, 2048)
	tmp := make([]byte, 1024)
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
				// Validate status.
				if !bytes.HasPrefix(buf, []byte("HTTP/1.1 101 ")) {
					return nil, fmt.Errorf("expected 101, got %q", buf[:min(32, len(buf))])
				}
				return buf[idx+4:], nil
			}
		}
		if err != nil {
			return buf, err
		}
	}
}

// drainUntilStreamEnd reads frames until a DATA frame with END_STREAM on the
// target stream is observed. Returns once observed or on timeout.
func drainUntilStreamEnd(c net.Conn, carry []byte, targetStream uint32, deadline time.Duration) error {
	_ = c.SetReadDeadline(time.Now().Add(deadline))
	buf := carry
	tmp := make([]byte, 4096)
	for {
		// Try to parse any complete frames in buf.
		for len(buf) >= 9 {
			length := int(buf[0])<<16 | int(buf[1])<<8 | int(buf[2])
			if len(buf) < 9+length {
				break
			}
			ftype := buf[3]
			flags := buf[4]
			sid := binary.BigEndian.Uint32(buf[5:9]) & 0x7fffffff
			buf = buf[9+length:]
			if ftype == frameData && sid == targetStream && flags&flagEndStream != 0 {
				return nil
			}
			if ftype == frameHeaders && sid == targetStream && flags&flagEndStream != 0 {
				// Some servers send body-less responses as a HEADERS with
				// END_STREAM. Accept that as stream end.
				return nil
			}
			if ftype == frameGoAway {
				return fmt.Errorf("GOAWAY received")
			}
		}
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			continue
		}
		if err != nil {
			return err
		}
	}
}

// doUpgradeRoundTrip performs one full handshake: dial + send upgrade +
// read 101 + drain server preface + read stream-1 response.
//
// Each iteration opens a fresh TCP conn. To avoid ephemeral-port exhaustion
// (each closed conn normally sits in TIME_WAIT for seconds), we set
// SO_LINGER=0 so Close() sends RST instead of FIN — skipping TIME_WAIT at
// the cost of treating the conn as "aborted" by the kernel. This is a
// standard benchmarking workaround and only affects the cleanup phase.
func doUpgradeRoundTrip(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	defer c.Close()
	// Upgrade request + client preface + empty SETTINGS frame in one write.
	msg := buildUpgradeRequest("/pong", "bench.local")
	msg = append(msg, []byte(h2cPreface)...)
	msg = append(msg, encodeFrame(frameSettings, 0, 0, nil)...)
	if _, err := c.Write(msg); err != nil {
		return err
	}
	tail, err := consumeH1_101(c, 2*time.Second)
	if err != nil {
		return err
	}
	return drainUntilStreamEnd(c, tail, 1, 2*time.Second)
}

// BenchmarkH2CUpgradeHandshake_Celeris measures the full upgrade dance on a
// fresh TCP conn against celeris.
func BenchmarkH2CUpgradeHandshake_Celeris(b *testing.B) {
	addr, stop := startCelerisH2C(b)
	defer stop()
	warmup(b, addr)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if err := doUpgradeRoundTrip(addr); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkH2CUpgradeHandshake_StdHTTP measures the same dance against
// net/http + x/net/http2/h2c.
func BenchmarkH2CUpgradeHandshake_StdHTTP(b *testing.B) {
	addr, stop := startStdH2C(b)
	defer stop()
	warmup(b, addr)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if err := doUpgradeRoundTrip(addr); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkH2CUpgradeHandshake_Celeris_Parallel saturates the server with
// concurrent fresh-conn handshakes.
func BenchmarkH2CUpgradeHandshake_Celeris_Parallel(b *testing.B) {
	addr, stop := startCelerisH2C(b)
	defer stop()
	warmup(b, addr)
	var errs int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := doUpgradeRoundTrip(addr); err != nil {
				atomic.AddInt64(&errs, 1)
			}
		}
	})
	if errs > 0 {
		b.Fatalf("errors: %d", errs)
	}
}

// BenchmarkH2CUpgradeHandshake_StdHTTP_Parallel is the std counterpart.
func BenchmarkH2CUpgradeHandshake_StdHTTP_Parallel(b *testing.B) {
	addr, stop := startStdH2C(b)
	defer stop()
	warmup(b, addr)
	var errs int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := doUpgradeRoundTrip(addr); err != nil {
				atomic.AddInt64(&errs, 1)
			}
		}
	})
	if errs > 0 {
		b.Fatalf("errors: %d", errs)
	}
}

// warmup sends one handshake so any lazy lazy-init cost (hpack tables,
// pooling warm-up) doesn't land in b.N.
func warmup(b *testing.B, addr string) {
	b.Helper()
	for i := 0; i < 3; i++ {
		if err := doUpgradeRoundTrip(addr); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}
}

// ---------- Steady-state (post-upgrade) benchmarks ----------

// establishH2Conn runs the RFC 7540 §3.2 dance and leaves the conn in H2
// mode with stream 1 complete. Subsequent requests use fresh odd stream IDs.
func establishH2Conn(addr string) (net.Conn, []byte, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	msg := buildUpgradeRequest("/pong", "bench.local")
	msg = append(msg, []byte(h2cPreface)...)
	msg = append(msg, encodeFrame(frameSettings, 0, 0, nil)...)
	if _, err := c.Write(msg); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	tail, err := consumeH1_101(c, 2*time.Second)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	if err := drainUntilStreamEnd(c, tail, 1, 2*time.Second); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	// Acknowledge the server's SETTINGS (already in the drained frames).
	// For simplicity we emit one SETTINGS ACK; the server usually sends an
	// empty SETTINGS during upgrade response which we should ack to stay
	// spec-compliant for subsequent requests.
	if _, err := c.Write(encodeFrame(frameSettings, flagAck, 0, nil)); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, nil, nil
}

// benchSteadyState drives N sequential GET /pong requests over a single
// already-upgraded H2 connection, using odd stream IDs 3, 5, 7, ...
func benchSteadyState(b *testing.B, addr string) {
	c, _, err := establishH2Conn(addr)
	if err != nil {
		b.Fatalf("establish: %v", err)
	}
	defer c.Close()
	var buf []byte
	streamID := uint32(3)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		frame := encodeHeadersFrame(streamID, "GET", "/pong", "bench.local")
		if _, err := c.Write(frame); err != nil {
			b.Fatal(err)
		}
		if err := drainUntilStreamEnd(c, buf, streamID, 2*time.Second); err != nil {
			b.Fatalf("drain stream %d: %v", streamID, err)
		}
		buf = buf[:0]
		streamID += 2
	}
}

func BenchmarkH2CSteadyState_Celeris(b *testing.B) {
	addr, stop := startCelerisH2C(b)
	defer stop()
	benchSteadyState(b, addr)
}

func BenchmarkH2CSteadyState_StdHTTP(b *testing.B) {
	addr, stop := startStdH2C(b)
	defer stop()
	benchSteadyState(b, addr)
}

// ---------- sanity / smoke tests (non-benchmark) ----------

// TestSanity_CelerisUpgrade ensures the celeris harness actually returns a
// 101 and a stream-1 response before any benchmark is trusted.
func TestSanity_CelerisUpgrade(t *testing.T) {
	addr, stop := startCelerisH2C(t)
	defer stop()
	if err := doUpgradeRoundTrip(addr); err != nil {
		t.Fatalf("celeris upgrade: %v", err)
	}
}

func TestSanity_StdHTTPUpgrade(t *testing.T) {
	addr, stop := startStdH2C(t)
	defer stop()
	if err := doUpgradeRoundTrip(addr); err != nil {
		t.Fatalf("stdhttp upgrade: %v", err)
	}
}

// small helpers -----------------------------------------------------------

var _ = io.EOF // keep io import stable across refactors
