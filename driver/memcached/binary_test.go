package memcached

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/goceleris/celeris/driver/memcached/protocol"
)

// fakeBinaryServer is a minimal in-process server that speaks the binary
// protocol. Unlike the text fake it only implements the subset the driver
// needs: get / set / delete / incr / decr / version / touch / flush.
type fakeBinaryServer struct {
	ln    net.Listener
	mu    sync.Mutex
	store map[string]binItem
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
		go f.serve(c)
	}
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
	// Binary GetMulti is sequential GETs; verify it still aggregates hits
	// and skips misses.
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
