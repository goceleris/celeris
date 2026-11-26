//go:build linux

package memcached

// A connection that the server drops while a command is in flight must fail the command with the
// close error (EOF, ErrClosed, a reset): never success, never a miss, never a partial multi-get
// presented as complete. The binary commands of the event-loop path used to return success: the
// drain of the closing conn finishes the request, waitTransport reported a finished request as nil,
// and the callers read binStatus == 0 == StatusOK.
//
// The fake server answers the binary handshake (VERSION) in full, reads one request, and writes the
// first N bytes of the valid reply that request deserves, then closes: N is the controlled byte count,
// chosen at the points where a drop is told apart (before any reply byte, inside the first line or
// header, inside the first value, between the replies of a multi-reply command, one byte short of the
// whole). Every command of the Client x text and binary x the loop path, a started std server and the
// direct path is a cell, each with its own key (the marker) and a watchdog.
//
// CELERIS_MC_DROP_FULL=1 runs every prefix length of every reply instead (long mode).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
)

// dropCmd is one command of the Client: run returns its error.
type dropCmd struct {
	name string
	run  func(ctx context.Context, c *Client, key string) error
}

func dropCommands() []dropCmd {
	return []dropCmd{
		{"Get", func(ctx context.Context, c *Client, k string) error { _, err := c.Get(ctx, k); return err }},
		{"GetBytes", func(ctx context.Context, c *Client, k string) error { _, err := c.GetBytes(ctx, k); return err }},
		{"Gets", func(ctx context.Context, c *Client, k string) error { _, err := c.Gets(ctx, k); return err }},
		{"GetMulti", func(ctx context.Context, c *Client, k string) error {
			_, err := c.GetMulti(ctx, k+"-a", k+"-b", k+"-c")
			return err
		}},
		{"GetMultiBytes", func(ctx context.Context, c *Client, k string) error {
			_, err := c.GetMultiBytes(ctx, k+"-a", k+"-b", k+"-c")
			return err
		}},
		{"Set", func(ctx context.Context, c *Client, k string) error { return c.Set(ctx, k, "v", time.Minute) }},
		{"SetBytes", func(ctx context.Context, c *Client, k string) error {
			return c.SetBytes(ctx, k, []byte("v"), time.Minute)
		}},
		{"Add", func(ctx context.Context, c *Client, k string) error { return c.Add(ctx, k, "v", time.Minute) }},
		{"Replace", func(ctx context.Context, c *Client, k string) error { return c.Replace(ctx, k, "v", time.Minute) }},
		{"Append", func(ctx context.Context, c *Client, k string) error { return c.Append(ctx, k, "v") }},
		{"Prepend", func(ctx context.Context, c *Client, k string) error { return c.Prepend(ctx, k, "v") }},
		{"CAS", func(ctx context.Context, c *Client, k string) error {
			_, err := c.CAS(ctx, k, "v", 7, time.Minute)
			return err
		}},
		{"Delete", func(ctx context.Context, c *Client, k string) error { return c.Delete(ctx, k) }},
		{"Incr", func(ctx context.Context, c *Client, k string) error { _, err := c.Incr(ctx, k, 1); return err }},
		{"Decr", func(ctx context.Context, c *Client, k string) error { _, err := c.Decr(ctx, k, 1); return err }},
		{"Touch", func(ctx context.Context, c *Client, k string) error { return c.Touch(ctx, k, time.Minute) }},
		{"Flush", func(ctx context.Context, c *Client, k string) error { return c.Flush(ctx) }},
		{"FlushAfter", func(ctx context.Context, c *Client, k string) error { return c.FlushAfter(ctx, time.Second) }},
		{"Stats", func(ctx context.Context, c *Client, k string) error { _, err := c.Stats(ctx); return err }},
		{"Version", func(ctx context.Context, c *Client, k string) error { _, err := c.Version(ctx); return err }},
		{"Ping", func(ctx context.Context, c *Client, k string) error { return c.Ping(ctx) }},
	}
}

// ---- the replies the fake would give ----

func le16(b []byte, v int) { binary.BigEndian.PutUint16(b, uint16(v)) }

// binPacket is one response packet.
func binPacket(op byte, opaque []byte, extras, key, value []byte) []byte {
	h := make([]byte, 24)
	h[0], h[1] = 0x81, op
	le16(h[2:], len(key))
	h[4] = byte(len(extras))
	binary.BigEndian.PutUint32(h[8:], uint32(len(extras)+len(key)+len(value)))
	copy(h[12:16], opaque)
	binary.BigEndian.PutUint64(h[16:], 7)
	out := append(h, extras...)
	out = append(out, key...)
	return append(out, value...)
}

type binReq struct {
	op     byte
	opaque []byte
	key    []byte
}

func parseBinRequests(b []byte) []binReq {
	var out []binReq
	for len(b) >= 24 {
		body := int(binary.BigEndian.Uint32(b[8:12]))
		kl, el := int(binary.BigEndian.Uint16(b[2:4])), int(b[4])
		if len(b) < 24+body || el+kl > body {
			break
		}
		out = append(out, binReq{op: b[1], opaque: b[12:16], key: b[24+el : 24+el+kl]})
		b = b[24+body:]
	}
	return out
}

// binReply is the valid reply to the request chunk b; first is where its first packet ends.
func binReply(b []byte) (reply []byte, first int) {
	reqs := parseBinRequests(b)
	if len(reqs) == 0 {
		return nil, 0
	}
	r := reqs[0]
	flags := []byte{0, 0, 0, 0}
	var out []byte
	switch r.op {
	case 0x00, 0x09: // get
		out = binPacket(r.op, r.opaque, flags, nil, []byte("hello"))
	case 0x0d, 0x0c: // getkq / getk: a multi-get
		for _, q := range reqs {
			if q.op == 0x0d {
				out = append(out, binPacket(q.op, q.opaque, flags, q.key, []byte("hello"))...)
			}
		}
		out = append(out, binPacket(0x0a, reqs[len(reqs)-1].opaque, nil, nil, nil)...)
		first = 24 + 4 + len(r.key) + 5
		return out, first
	case 0x05, 0x06: // incr, decr
		out = binPacket(r.op, r.opaque, nil, nil, []byte{0, 0, 0, 0, 0, 0, 0, 5})
	case 0x0b: // version
		out = binPacket(r.op, r.opaque, nil, nil, []byte("1.6.41"))
	case 0x10: // stats: two entries and the terminator
		p1 := binPacket(r.op, r.opaque, nil, []byte("pid"), []byte("1"))
		out = append(p1, binPacket(r.op, r.opaque, nil, []byte("uptime"), []byte("2"))...)
		out = append(out, binPacket(r.op, r.opaque, nil, nil, nil)...)
		return out, len(p1)
	default: // set, add, replace, append, prepend, delete, touch, flush: header only
		out = binPacket(r.op, r.opaque, nil, nil, nil)
	}
	return out, len(out)
}

// textReply is the valid reply to the request line l; first is where its first unit ends.
func textReply(l string) (reply []byte, first int) {
	f := strings.Fields(l)
	if len(f) == 0 {
		return nil, 0
	}
	value := func(k string, cas bool) string {
		if cas {
			return "VALUE " + k + " 0 5 7\r\nhello\r\n"
		}
		return "VALUE " + k + " 0 5\r\nhello\r\n"
	}
	switch f[0] {
	case "get", "gets":
		var s string
		for _, k := range f[1:] {
			s += value(k, f[0] == "gets")
		}
		first = len(value(f[1], f[0] == "gets"))
		return []byte(s + "END\r\n"), first
	case "stats":
		s := "STAT pid 1\r\nSTAT uptime 2\r\nEND\r\n"
		return []byte(s), len("STAT pid 1\r\n")
	case "incr", "decr":
		return []byte("5\r\n"), 3
	case "delete":
		return []byte("DELETED\r\n"), 9
	case "touch":
		return []byte("TOUCHED\r\n"), 9
	case "flush_all":
		return []byte("OK\r\n"), 4
	case "version":
		return []byte("VERSION 1.6.41\r\n"), 16
	default: // set, add, replace, append, prepend, cas
		return []byte("STORED\r\n"), 8
	}
}

// cuts are the byte counts the fake writes before it drops the conn.
func dropCuts(reply []byte, first int, bin bool, full bool) []int {
	n := len(reply)
	set := map[int]bool{}
	if full {
		for i := 0; i < n; i++ {
			set[i] = true
		}
	} else {
		set[0] = true // before any reply byte
		if bin {
			set[min(12, n-1)] = true // half a header
			if first > 24 {
				set[24+(first-24)/2] = true // half a value
			} else {
				set[23] = true
			}
		} else {
			set[min(3, n-1)] = true     // inside the first line
			set[max(first-3, 1)] = true // inside the first value
		}
		if first < n {
			set[first] = true // between the replies
			if first+1 < n {
				set[first+1] = true
			}
		}
		set[n-1] = true // one byte short
	}
	var out []int
	for c := range set {
		if c >= 0 && c < n {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}

// dropServer serves one request per conn and drops it after cut(reply) bytes of its reply. For the
// binary protocol the VERSION handshake is answered in full first.
func dropServer(t *testing.T, bin bool, cutOf func(reply []byte, first int) int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 64<<10)
				if bin {
					// The handshake: one VERSION packet.
					if _, err := io.ReadFull(c, buf[:24]); err != nil {
						return
					}
					_, _ = c.Write(binPacket(0x0b, buf[12:16], nil, nil, []byte("1.6.41")))
				}
				n, err := c.Read(buf)
				if err != nil || n == 0 {
					return
				}
				var reply []byte
				var first int
				if bin {
					reply, first = binReply(buf[:n])
				} else {
					reply, first = textReply(strings.SplitN(string(buf[:n]), "\r\n", 2)[0])
				}
				if cut := cutOf(reply, first); cut > 0 {
					_, _ = c.Write(reply[:cut])
				}
				// A clean FIN after the partial reply, then wait for the client to go.
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func isDropError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func TestDroppedConnFailsEveryCommand(t *testing.T) {
	full := os.Getenv("CELERIS_MC_DROP_FULL") == "1"
	std := looptest.StartStd(t)
	paths := []struct {
		name string
		opts func() []Option
	}{
		{"loop", func() []Option { return []Option{WithEngine(looptest.Unstarted())} }},
		{"std", func() []Option { return []Option{WithEngine(std)} }},
		{"direct", func() []Option { return nil }},
	}
	for _, proto := range []Protocol{ProtocolText, ProtocolBinary} {
		bin := proto == ProtocolBinary
		for _, cmd := range dropCommands() {
			// The cut points of this command's reply, from a probe of the fake's own reply.
			key := "drop-" + strings.ToLower(cmd.name)
			cutPoints := probeCuts(t, proto, cmd, key, full)
			for _, cut := range cutPoints {
				for _, p := range paths {
					name := fmt.Sprintf("%v/%s/cut=%d/%s", proto, cmd.name, cut, p.name)
					t.Run(name, func(t *testing.T) {
						addr := dropServer(t, bin, func(reply []byte, first int) int { return cut })
						opts := append([]Option{WithProtocol(proto), WithDialTimeout(2 * time.Second)}, p.opts()...)
						c, err := NewClient(addr, opts...)
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = c.Close() }()
						ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
						defer cancel()
						done := make(chan error, 1)
						go func() { done <- cmd.run(ctx, c, key) }()
						select {
						case err := <-done:
							switch {
							case err == nil:
								t.Fatalf("%s(%s) returned success after the server dropped the conn at byte %d of its reply", cmd.name, key, cut)
							case errors.Is(err, ErrCacheMiss), errors.Is(err, ErrNotStored), errors.Is(err, ErrCASConflict):
								t.Fatalf("%s(%s) returned %v (an answer) after the drop at byte %d", cmd.name, key, err, cut)
							case !isDropError(err):
								t.Fatalf("%s(%s) returned %v after the drop at byte %d, want the close error (EOF, ErrClosed, reset)", cmd.name, key, err, cut)
							}
						case <-time.After(8 * time.Second):
							t.Fatalf("%s(%s): no return 8 s after the drop at byte %d (hung)", cmd.name, key, cut)
						}
					})
				}
			}
		}
	}
}

// probeCuts asks the fake what reply it would give cmd (by running the fake's builders on the request
// the Client sends) and returns the cut points. It records the request through a sniffing server.
func probeCuts(t *testing.T, proto Protocol, cmd dropCmd, key string, full bool) []int {
	t.Helper()
	bin := proto == ProtocolBinary
	var mu sync.Mutex
	var cuts []int
	addr := dropServer(t, bin, func(reply []byte, first int) int {
		mu.Lock()
		cuts = dropCuts(reply, first, bin, full)
		mu.Unlock()
		return 0
	})
	c, err := NewClient(addr, WithProtocol(proto), WithDialTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cmd.run(ctx, c, key)
	cancel()
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(cuts) == 0 {
		t.Fatalf("%v %s: the fake saw no request", proto, cmd.name)
	}
	return cuts
}

// The binary handshake (VERSION) is a command too: a server that drops the conn before, inside or
// one byte short of its reply fails the dial, so the first command returns the close error.
func TestDroppedConnFailsTheHandshake(t *testing.T) {
	std := looptest.StartStd(t)
	paths := []struct {
		name string
		opts func() []Option
	}{
		{"loop", func() []Option { return []Option{WithEngine(looptest.Unstarted())} }},
		{"std", func() []Option { return []Option{WithEngine(std)} }},
		{"direct", func() []Option { return nil }},
	}
	full := binPacket(0x0b, []byte{0, 0, 0, 1}, nil, nil, []byte("1.6.41"))
	for _, cut := range dropCuts(full, len(full), true, os.Getenv("CELERIS_MC_DROP_FULL") == "1") {
		for _, p := range paths {
			t.Run(fmt.Sprintf("cut=%d/%s", cut, p.name), func(t *testing.T) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				go func() {
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						go func(c net.Conn) {
							defer func() { _ = c.Close() }()
							_ = c.SetDeadline(time.Now().Add(10 * time.Second))
							buf := make([]byte, 24)
							if _, err := io.ReadFull(c, buf); err != nil {
								return
							}
							reply := binPacket(0x0b, buf[12:16], nil, nil, []byte("1.6.41"))
							if cut > 0 {
								_, _ = c.Write(reply[:cut])
							}
							if tc, ok := c.(*net.TCPConn); ok {
								_ = tc.CloseWrite()
							}
							_, _ = io.Copy(io.Discard, c)
						}(c)
					}
				}()
				opts := append([]Option{WithProtocol(ProtocolBinary), WithDialTimeout(2 * time.Second)}, p.opts()...)
				c, err := NewClient(ln.Addr().String(), opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.Close() }()
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				_, err = c.Get(ctx, "drop-handshake")
				if err == nil || !isDropError(err) {
					t.Fatalf("Get after a handshake dropped at byte %d returned %v, want the close error", cut, err)
				}
			})
		}
	}
}
