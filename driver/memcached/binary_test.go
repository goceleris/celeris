package memcached

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// fakeBinaryServer is a minimal in-process server that speaks the binary
// protocol. Unlike the text fake it only implements the subset the driver
// needs: get / set / delete / incr / decr / version / touch / flush / stats,
// plus OpGetKQ + OpNoop pipelined multi-get.
type fakeBinaryServer struct {
	ln    net.Listener
	mu    sync.Mutex
	store map[string]binItem

	// statsReply, if non-nil, overrides the default set of STAT entries
	// emitted in response to OpStat.
	statsReply []statKV

	// writeCount counts the number of discrete c.Write calls the fake has
	// made back to the client. A pipelined GetMulti batch is written with a
	// single Write (one trip through bytes.Buffer). Atomic so tests can read
	// it without grabbing mu.
	writeCount atomic.Uint64
	// recvBatchSizes records the length of each client read this conn saw.
	// Used to verify the driver wrote one pipelined batch rather than N
	// individual packets.
	recvBatchSizes []int
	// lastRecv captures every client read for byte-level framing assertions.
	lastRecv []byte
}

// statKV is one scripted STAT reply entry.
type statKV struct {
	name  string
	value string
}

// statsReplyOrDefault returns the configured stats script, or a default set
// of three well-known entries if the test did not install an override.
func (f *fakeBinaryServer) statsReplyOrDefault() []statKV {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsReply != nil {
		out := make([]statKV, len(f.statsReply))
		copy(out, f.statsReply)
		return out
	}
	return []statKV{
		{"pid", "1234"},
		{"version", "1.6.celeris-fake-bin"},
		{"uptime", "42"},
	}
}

type binItem struct {
	value []byte
	flags uint32
	cas   uint64
}

func startFakeBinary(t *testing.T) *fakeBinaryServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBinaryServer{ln: ln, store: map[string]binItem{}}
	t.Cleanup(func() { _ = ln.Close() })
	go f.accept()
	return f
}

// Addr returns "host:port".
func (f *fakeBinaryServer) Addr() string { return f.ln.Addr().String() }

func (f *fakeBinaryServer) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(&trackedBinConn{Conn: c, f: f})
	}
}

// trackedBinConn wraps net.Conn so the fake can observe how many Read calls
// the driver's write pattern produces on the server side (1 read per client
// Write is the fast-path invariant for pipelined GetMulti) and how many
// discrete c.Write calls the fake made back.
type trackedBinConn struct {
	net.Conn
	f *fakeBinaryServer
}

func (t *trackedBinConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.f.mu.Lock()
		t.f.recvBatchSizes = append(t.f.recvBatchSizes, n)
		t.f.lastRecv = append(t.f.lastRecv, p[:n]...)
		t.f.mu.Unlock()
	}
	return n, err
}

func (t *trackedBinConn) Write(p []byte) (int, error) {
	t.f.writeCount.Add(1)
	return t.Conn.Write(p)
}

func (f *fakeBinaryServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	hdr := make([]byte, protocol.BinHeaderLen)
	var casCtr uint64
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		if hdr[0] != protocol.MagicRequest {
			return
		}
		op := hdr[1]
		keyLen := int(binary.BigEndian.Uint16(hdr[2:4]))
		extrasLen := int(hdr[4])
		bodyLen := int(binary.BigEndian.Uint32(hdr[8:12]))
		opaque := binary.BigEndian.Uint32(hdr[12:16])
		cas := binary.BigEndian.Uint64(hdr[16:24])
		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		extras := body[:extrasLen]
		key := string(body[extrasLen : extrasLen+keyLen])
		value := body[extrasLen+keyLen:]

		resp := &bytes.Buffer{}
		switch op {
		case protocol.OpVersion:
			writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, []byte("1.6.celeris-fake-bin"))
		case protocol.OpGet:
			f.mu.Lock()
			it, ok := f.store[key]
			f.mu.Unlock()
			if !ok {
				writeResp(resp, op, protocol.StatusKeyNotFound, 0, opaque, nil, nil, []byte("Not found"))
			} else {
				var ex [4]byte
				binary.BigEndian.PutUint32(ex[0:4], it.flags)
				writeResp(resp, op, protocol.StatusOK, it.cas, opaque, ex[:], nil, it.value)
			}
		case protocol.OpGetKQ:
			// Quiet get-with-key: reply on hit (opcode echo, extras+key+value),
			// suppress on miss.
			f.mu.Lock()
			it, ok := f.store[key]
			f.mu.Unlock()
			if ok {
				var ex [4]byte
				binary.BigEndian.PutUint32(ex[0:4], it.flags)
				writeResp(resp, op, protocol.StatusOK, it.cas, opaque, ex[:], []byte(key), it.value)
			}
		case protocol.OpNoop:
			writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, nil)
		case protocol.OpStat:
			// Emit a fixed scripted set of stat packets followed by the
			// zero-key terminator. The caller can install a custom statsReply
			// hook (see fakeBinaryServer.statsReply) to override the packet
			// list per test.
			entries := f.statsReplyOrDefault()
			for _, e := range entries {
				writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, []byte(e.name), []byte(e.value))
			}
			writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, nil)
		case protocol.OpSet, protocol.OpAdd, protocol.OpReplace:
			var flags, exptime uint32
			if len(extras) >= 8 {
				flags = binary.BigEndian.Uint32(extras[0:4])
				exptime = binary.BigEndian.Uint32(extras[4:8])
			}
			_ = exptime
			f.mu.Lock()
			existing, present := f.store[key]
			status := protocol.StatusOK
			if op == protocol.OpAdd && present {
				status = protocol.StatusKeyExists
			} else if op == protocol.OpReplace && !present {
				status = protocol.StatusKeyNotFound
			} else if op == protocol.OpSet && cas != 0 && present && existing.cas != cas {
				status = protocol.StatusKeyExists
			} else if op == protocol.OpSet && cas != 0 && !present {
				status = protocol.StatusKeyNotFound
			}
			if status == protocol.StatusOK {
				casCtr++
				f.store[key] = binItem{value: append([]byte(nil), value...), flags: flags, cas: casCtr}
				f.mu.Unlock()
				writeResp(resp, op, protocol.StatusOK, casCtr, opaque, nil, nil, nil)
			} else {
				f.mu.Unlock()
				writeResp(resp, op, status, 0, opaque, nil, nil, []byte("denied"))
			}
		case protocol.OpDelete:
			f.mu.Lock()
			_, ok := f.store[key]
			if ok {
				delete(f.store, key)
			}
			f.mu.Unlock()
			if ok {
				writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, nil)
			} else {
				writeResp(resp, op, protocol.StatusKeyNotFound, 0, opaque, nil, nil, []byte("not found"))
			}
		case protocol.OpIncrement, protocol.OpDecrement:
			var delta uint64
			if len(extras) >= 8 {
				delta = binary.BigEndian.Uint64(extras[0:8])
			}
			f.mu.Lock()
			it, ok := f.store[key]
			if !ok {
				f.mu.Unlock()
				writeResp(resp, op, protocol.StatusKeyNotFound, 0, opaque, nil, nil, []byte("not found"))
				_, _ = c.Write(resp.Bytes())
				continue
			}
			var cur uint64
			for _, b := range it.value {
				if b < '0' || b > '9' {
					cur = 0
					break
				}
				cur = cur*10 + uint64(b-'0')
			}
			if op == protocol.OpIncrement {
				cur += delta
			} else {
				if delta > cur {
					cur = 0
				} else {
					cur -= delta
				}
			}
			casCtr++
			it.cas = casCtr
			// Binary incr returns the new value as 8-byte big-endian.
			var v [8]byte
			binary.BigEndian.PutUint64(v[0:8], cur)
			// Store it as 8-byte BE too so subsequent reads stay valid arithmetic.
			it.value = []byte{}
			// Convert cur back to decimal ASCII for stored value.
			if cur == 0 {
				it.value = []byte("0")
			} else {
				var buf [20]byte
				i := len(buf)
				n := cur
				for n > 0 {
					i--
					buf[i] = byte('0' + n%10)
					n /= 10
				}
				it.value = append([]byte{}, buf[i:]...)
			}
			f.store[key] = it
			f.mu.Unlock()
			writeResp(resp, op, protocol.StatusOK, casCtr, opaque, nil, nil, v[:])
		case protocol.OpTouch:
			f.mu.Lock()
			_, ok := f.store[key]
			f.mu.Unlock()
			if !ok {
				writeResp(resp, op, protocol.StatusKeyNotFound, 0, opaque, nil, nil, []byte("not found"))
			} else {
				writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, nil)
			}
		case protocol.OpFlush:
			f.mu.Lock()
			f.store = map[string]binItem{}
			f.mu.Unlock()
			writeResp(resp, op, protocol.StatusOK, 0, opaque, nil, nil, nil)
		case protocol.OpQuit:
			return
		default:
			writeResp(resp, op, protocol.StatusUnknownCommand, 0, opaque, nil, nil, []byte("unknown"))
		}
		if _, err := c.Write(resp.Bytes()); err != nil {
			return
		}
	}
}

func writeResp(w *bytes.Buffer, op byte, status uint16, cas uint64, opaque uint32, extras, key, value []byte) {
	var hdr [protocol.BinHeaderLen]byte
	hdr[0] = protocol.MagicResponse
	hdr[1] = op
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(key)))
	hdr[4] = byte(len(extras))
	binary.BigEndian.PutUint16(hdr[6:8], status)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(extras)+len(key)+len(value)))
	binary.BigEndian.PutUint32(hdr[12:16], opaque)
	binary.BigEndian.PutUint64(hdr[16:24], cas)
	w.Write(hdr[:])
	w.Write(extras)
	w.Write(key)
	w.Write(value)
}

func newBinaryClient(t *testing.T) (*Client, *fakeBinaryServer) {
	t.Helper()
	fake := startFakeBinary(t)
	c, err := NewClient(fake.Addr(), WithProtocol(ProtocolBinary))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fake
}

func TestBinarySetGet(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" {
		t.Fatalf("got %q", got)
	}
}

func TestBinaryGetMiss(t *testing.T) {
	c, _ := newBinaryClient(t)
	_, err := c.Get(context.Background(), "absent")
	if !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestBinaryAddReplace(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	if err := c.Add(ctx, "k", "v1", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(ctx, "k", "v2", 0); !errors.Is(err, ErrCASConflict) {
		// Binary Add returns StatusKeyExists which we map to ErrCASConflict.
		// This is an intentional divergence from text; tests just need to
		// verify it fails.
		t.Fatalf("expected ErrCASConflict, got %v", err)
	}
	if err := c.Replace(ctx, "missing", "x", 0); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestBinaryDelete(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "k", "v", 0)
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestBinaryIncrDecr(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "ctr", "10", 0)
	n, err := c.Incr(ctx, "ctr", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 15 {
		t.Fatalf("got %d", n)
	}
	n, err = c.Decr(ctx, "ctr", 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 12 {
		t.Fatalf("got %d", n)
	}
}

func TestBinaryVersion(t *testing.T) {
	c, _ := newBinaryClient(t)
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v == "" {
		t.Fatal("empty version")
	}
}

func TestBinaryTouch(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "k", "v", 0)
	if err := c.Touch(ctx, "k", 60); err != nil {
		t.Fatal(err)
	}
	if err := c.Touch(ctx, "missing", 60); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestBinaryFlush(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "k", "v", 0)
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}
}

func TestBinaryGetMulti(t *testing.T) {
	// Binary GetMulti is pipelined (OpGetKQ + OpNoop); verify it aggregates
	// hits and skips misses.
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	_ = c.Set(ctx, "a", "va", 0)
	_ = c.Set(ctx, "b", "vb", 0)
	out, err := c.GetMulti(ctx, "a", "b", "absent")
	if err != nil {
		t.Fatal(err)
	}
	if out["a"] != "va" || out["b"] != "vb" {
		t.Fatalf("got %#v", out)
	}
	if _, ok := out["absent"]; ok {
		t.Fatalf("absent key should be omitted")
	}
}

// TestBinary_GetMulti_Pipelined verifies that GetMulti issues one pipelined
// write containing N OpGetKQ packets followed by a single OpNoop terminator,
// rather than N individual round trips. We inspect the raw bytes the fake
// received on the wire for the precise request framing.
func TestBinary_GetMulti_Pipelined(t *testing.T) {
	c, fake := newBinaryClient(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, k, "v"+k, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Reset the on-wire recorder to isolate the GetMulti call.
	fake.mu.Lock()
	fake.lastRecv = fake.lastRecv[:0]
	fake.mu.Unlock()

	out, err := c.GetMulti(ctx, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out["a"] != "va" || out["b"] != "vb" || out["c"] != "vc" {
		t.Fatalf("GetMulti = %#v", out)
	}
	fake.mu.Lock()
	wire := append([]byte(nil), fake.lastRecv...)
	fake.mu.Unlock()
	opcodes := extractRequestOpcodes(t, wire)
	// Expect exactly OpGetKQ x3 then OpNoop x1.
	want := []byte{protocol.OpGetKQ, protocol.OpGetKQ, protocol.OpGetKQ, protocol.OpNoop}
	if !bytes.Equal(opcodes, want) {
		t.Fatalf("wire opcodes = %x, want %x", opcodes, want)
	}
}

// TestBinary_GetMulti_AllMisses verifies that a pipelined batch where every
// key is absent returns an empty map (and no error) — the driver must still
// recognize the Noop terminator as a normal completion.
func TestBinary_GetMulti_AllMisses(t *testing.T) {
	c, _ := newBinaryClient(t)
	out, err := c.GetMulti(context.Background(), "x", "y", "z")
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty map, got %#v", out)
	}
}

// TestBinary_GetMulti_PartialHits sets 3 of 5 keys and verifies only the
// hits come back in the response map.
func TestBinary_GetMulti_PartialHits(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	keys := []string{"pk0", "pk1", "pk2", "pk3", "pk4"}
	hits := map[string]string{"pk0": "v0", "pk2": "v2", "pk4": "v4"}
	for k, v := range hits {
		if err := c.Set(ctx, k, v, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetMulti(ctx, keys...)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(hits) {
		t.Fatalf("GetMulti len = %d (%v), want %d (%v)", len(got), got, len(hits), hits)
	}
	for k, v := range hits {
		if got[k] != v {
			t.Fatalf("GetMulti[%q] = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"pk1", "pk3"} {
		if _, ok := got[k]; ok {
			t.Fatalf("miss key %q should not appear in result", k)
		}
	}
}

// TestBinary_GetMulti_LargeBatch sets and retrieves 100 keys in a single
// pipelined round trip. Exercises buffer-growth paths in the writer and
// stream-terminator detection across many server replies.
func TestBinary_GetMulti_LargeBatch(t *testing.T) {
	c, _ := newBinaryClient(t)
	ctx := context.Background()
	const N = 100
	keys := make([]string, N)
	expected := make(map[string]string, N)
	for i := 0; i < N; i++ {
		k := "big_" + strconv.Itoa(i)
		v := "val_" + strconv.Itoa(i)
		keys[i] = k
		expected[k] = v
		if err := c.Set(ctx, k, v, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetMulti(ctx, keys...)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != N {
		t.Fatalf("GetMulti returned %d entries, want %d", len(got), N)
	}
	for k, v := range expected {
		if got[k] != v {
			t.Fatalf("GetMulti[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestBinary_Stats_MultiPacket verifies that Stats accumulates every STAT
// packet the server emits until the zero-key terminator arrives.
func TestBinary_Stats_MultiPacket(t *testing.T) {
	c, fake := newBinaryClient(t)
	fake.mu.Lock()
	fake.statsReply = []statKV{
		{"pid", "42"},
		{"version", "1.6.fake"},
		{"curr_items", "7"},
		{"cmd_get", "0"},
		{"cmd_set", "1"},
	}
	fake.mu.Unlock()
	out, err := c.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("Stats returned %d entries (%v), want 5", len(out), out)
	}
	if out["pid"] != "42" || out["version"] != "1.6.fake" ||
		out["curr_items"] != "7" || out["cmd_get"] != "0" || out["cmd_set"] != "1" {
		t.Fatalf("Stats values mismatch: %v", out)
	}
}

// TestBinary_Stats_Empty verifies that an empty stats stream (server replies
// with only the terminator) returns an empty map, not an error.
func TestBinary_Stats_Empty(t *testing.T) {
	c, fake := newBinaryClient(t)
	fake.mu.Lock()
	fake.statsReply = []statKV{}
	fake.mu.Unlock()
	out, err := c.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("Stats returned %d entries (%v), want 0", len(out), out)
	}
}

// extractRequestOpcodes walks wire bytes parsed as a stream of binary
// request headers and returns the opcode sequence. Helper for framing
// assertions in the pipelined GetMulti test.
func extractRequestOpcodes(t *testing.T, wire []byte) []byte {
	t.Helper()
	var ops []byte
	i := 0
	for i+protocol.BinHeaderLen <= len(wire) {
		if wire[i] != protocol.MagicRequest {
			t.Fatalf("non-request magic at offset %d: 0x%02x", i, wire[i])
		}
		ops = append(ops, wire[i+1])
		bodyLen := int(binary.BigEndian.Uint32(wire[i+8 : i+12]))
		i += protocol.BinHeaderLen + bodyLen
	}
	if i != len(wire) {
		t.Fatalf("trailing bytes at offset %d (wire len %d)", i, len(wire))
	}
	return ops
}

// BenchmarkBinary_GetMulti_10 measures allocations for a 10-key pipelined
// binary GetMulti against the in-process fake. Reported for the v1.4.0
// alloc-budget tracker (task #58). A single pipelined round trip should not
// regress versus the earlier sequential implementation.
func BenchmarkBinary_GetMulti_10(b *testing.B) {
	fake := startFakeBinaryB(b)
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
	c, err := NewClient(fake.Addr(), WithProtocol(ProtocolBinary))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	for _, k := range keys {
		if err := c.Set(ctx, k, "v", 0); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := c.GetMulti(ctx, keys...); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.GetMulti(ctx, keys...); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBinary_Stats measures allocations for a native-binary Stats call
// against the in-process fake. Useful as a tripwire against regressions in
// the multi-packet accumulation path.
func BenchmarkBinary_Stats(b *testing.B) {
	fake := startFakeBinaryB(b)
	c, err := NewClient(fake.Addr(), WithProtocol(ProtocolBinary))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if _, err := c.Stats(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Stats(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// startFakeBinaryB is the *testing.B variant of startFakeBinary.
func startFakeBinaryB(b *testing.B) *fakeBinaryServer {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	f := &fakeBinaryServer{ln: ln, store: map[string]binItem{}}
	b.Cleanup(func() { _ = ln.Close() })
	go f.accept()
	return f
}
