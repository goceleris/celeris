package memcached

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMemcached is a minimal in-process memcached server used by tests. It
// speaks the text protocol only (binary tests drive the binary protocol
// package directly — see protocol_test.go). The handler allows per-test
// behavior to be injected; unset handlers fall back to the in-memory store.
type fakeMemcached struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	store *memStore
	// cmdCount is the number of commands this fake has received since start.
	// Atomic so cluster tests can assert request distribution across fakes
	// without synchronizing with the serve loop.
	cmdCount atomic.Uint64
	// getKeys is the total number of key arguments seen across all get/gets
	// requests. Useful for asserting fan-out proportions in the cluster
	// driver's GetMulti, where a single request on the wire carries N keys.
	getKeys atomic.Uint64
}

// memStore is a goroutine-safe map that services the fake server's default
// handler. Keys, values, flags, and CAS tokens round-trip through the fake
// exactly like a real memcached (minus sophisticated eviction policies).
type memStore struct {
	mu     sync.Mutex
	kv     map[string]memItem
	casCtr uint64
}

type memItem struct {
	value    []byte
	flags    uint32
	cas      uint64
	expireAt time.Time // zero means no expiration
}

func newMemStore() *memStore {
	return &memStore{kv: map[string]memItem{}}
}

// nextCAS returns a fresh, strictly-increasing CAS token. Caller holds s.mu.
func (s *memStore) nextCAS() uint64 {
	s.casCtr++
	return s.casCtr
}

// get returns (item, ok) after evicting expired entries. Caller must NOT
// hold s.mu.
func (s *memStore) get(key string) (memItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.kv[key]
	if !ok {
		return memItem{}, false
	}
	if !it.expireAt.IsZero() && time.Now().After(it.expireAt) {
		delete(s.kv, key)
		return memItem{}, false
	}
	return it, true
}

// expiration converts the wire-level exptime back into an absolute time.
// Matches memcached server semantics: 0 = no expiration, 1..2592000 =
// relative seconds, > 2592000 = absolute unix timestamp.
func toAbsoluteExpiry(exp int64) time.Time {
	if exp <= 0 {
		return time.Time{}
	}
	const relCap = 60 * 60 * 24 * 30
	if exp > relCap {
		return time.Unix(exp, 0)
	}
	return time.Now().Add(time.Duration(exp) * time.Second)
}

// startFake spawns the server and registers a cleanup hook.
func startFake(t *testing.T) *fakeMemcached {
	t.Helper()
	return startFakeTB(t)
}

// startFakeB is the benchmark variant.
func startFakeB(b *testing.B) *fakeMemcached {
	b.Helper()
	return startFakeTB(b)
}

func startFakeTB(tb testing.TB) *fakeMemcached {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	f := &fakeMemcached{ln: ln, store: newMemStore()}
	tb.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		conns := f.conns
		f.conns = nil
		f.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go f.accept()
	return f
}

// Addr returns "host:port".
func (f *fakeMemcached) Addr() string { return f.ln.Addr().String() }

func (f *fakeMemcached) accept() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.serve(c)
	}
}

// serve reads commands and dispatches them. Storage commands consume the
// data block on the following line; retrieval commands write VALUE...END.
func (f *fakeMemcached) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		cmd := fields[0]
		f.cmdCount.Add(1)
		switch cmd {
		case "get", "gets":
			f.getKeys.Add(uint64(len(fields) - 1))
			f.handleGet(cmd, fields[1:], w)
		case "set", "add", "replace", "append", "prepend":
			if !f.handleStore(cmd, fields[1:], r, w) {
				return
			}
		case "cas":
			if !f.handleCAS(fields[1:], r, w) {
				return
			}
		case "delete":
			f.handleDelete(fields[1:], w)
		case "incr", "decr":
			f.handleArith(cmd, fields[1:], w)
		case "touch":
			f.handleTouch(fields[1:], w)
		case "flush_all":
			f.handleFlushAll(fields[1:], w)
		case "version":
			_, _ = w.WriteString("VERSION 1.6.celeris-fake\r\n")
		case "stats":
			f.handleStats(w)
		case "quit":
			_ = w.Flush()
			return
		default:
			_, _ = w.WriteString("ERROR\r\n")
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func (f *fakeMemcached) handleGet(cmd string, keys []string, w *bufio.Writer) {
	for _, k := range keys {
		it, ok := f.store.get(k)
		if !ok {
			continue
		}
		_, _ = w.WriteString("VALUE ")
		_, _ = w.WriteString(k)
		_, _ = w.WriteString(" ")
		_, _ = w.WriteString(strconv.FormatUint(uint64(it.flags), 10))
		_, _ = w.WriteString(" ")
		_, _ = w.WriteString(strconv.Itoa(len(it.value)))
		if cmd == "gets" {
			_, _ = w.WriteString(" ")
			_, _ = w.WriteString(strconv.FormatUint(it.cas, 10))
		}
		_, _ = w.WriteString("\r\n")
		_, _ = w.Write(it.value)
		_, _ = w.WriteString("\r\n")
	}
	_, _ = w.WriteString("END\r\n")
}

// readN reads exactly n bytes followed by CRLF.
func readBlock(r *bufio.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if _, err := r.Discard(2); err != nil {
		return nil, err
	}
	return buf, nil
}

func (f *fakeMemcached) handleStore(cmd string, args []string, r *bufio.Reader, w *bufio.Writer) bool {
	if len(args) < 4 {
		_, _ = w.WriteString("CLIENT_ERROR bad command line\r\n")
		return true
	}
	key := args[0]
	flags, _ := strconv.ParseUint(args[1], 10, 32)
	exp, _ := strconv.ParseInt(args[2], 10, 64)
	n, _ := strconv.Atoi(args[3])
	// noreply may be args[4]
	noreply := len(args) >= 5 && args[4] == "noreply"
	data, err := readBlock(r, n)
	if err != nil {
		return false
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	existing, hasExisting := f.store.kv[key]
	switch cmd {
	case "set":
		f.store.kv[key] = memItem{
			value:    data,
			flags:    uint32(flags),
			cas:      f.store.nextCAS(),
			expireAt: toAbsoluteExpiry(exp),
		}
		if !noreply {
			_, _ = w.WriteString("STORED\r\n")
		}
	case "add":
		if hasExisting {
			if !noreply {
				_, _ = w.WriteString("NOT_STORED\r\n")
			}
			return true
		}
		f.store.kv[key] = memItem{
			value:    data,
			flags:    uint32(flags),
			cas:      f.store.nextCAS(),
			expireAt: toAbsoluteExpiry(exp),
		}
		if !noreply {
			_, _ = w.WriteString("STORED\r\n")
		}
	case "replace":
		if !hasExisting {
			if !noreply {
				_, _ = w.WriteString("NOT_STORED\r\n")
			}
			return true
		}
		f.store.kv[key] = memItem{
			value:    data,
			flags:    uint32(flags),
			cas:      f.store.nextCAS(),
			expireAt: toAbsoluteExpiry(exp),
		}
		if !noreply {
			_, _ = w.WriteString("STORED\r\n")
		}
	case "append":
		if !hasExisting {
			if !noreply {
				_, _ = w.WriteString("NOT_STORED\r\n")
			}
			return true
		}
		existing.value = append(existing.value, data...)
		existing.cas = f.store.nextCAS()
		f.store.kv[key] = existing
		if !noreply {
			_, _ = w.WriteString("STORED\r\n")
		}
	case "prepend":
		if !hasExisting {
			if !noreply {
				_, _ = w.WriteString("NOT_STORED\r\n")
			}
			return true
		}
		existing.value = append(data, existing.value...)
		existing.cas = f.store.nextCAS()
		f.store.kv[key] = existing
		if !noreply {
			_, _ = w.WriteString("STORED\r\n")
		}
	}
	return true
}

func (f *fakeMemcached) handleCAS(args []string, r *bufio.Reader, w *bufio.Writer) bool {
	if len(args) < 5 {
		_, _ = w.WriteString("CLIENT_ERROR bad command line\r\n")
		return true
	}
	key := args[0]
	flags, _ := strconv.ParseUint(args[1], 10, 32)
	exp, _ := strconv.ParseInt(args[2], 10, 64)
	n, _ := strconv.Atoi(args[3])
	cas, _ := strconv.ParseUint(args[4], 10, 64)
	noreply := len(args) >= 6 && args[5] == "noreply"
	data, err := readBlock(r, n)
	if err != nil {
		return false
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	existing, ok := f.store.kv[key]
	if !ok {
		if !noreply {
			_, _ = w.WriteString("NOT_FOUND\r\n")
		}
		return true
	}
	if existing.cas != cas {
		if !noreply {
			_, _ = w.WriteString("EXISTS\r\n")
		}
		return true
	}
	f.store.kv[key] = memItem{
		value:    data,
		flags:    uint32(flags),
		cas:      f.store.nextCAS(),
		expireAt: toAbsoluteExpiry(exp),
	}
	if !noreply {
		_, _ = w.WriteString("STORED\r\n")
	}
	return true
}

func (f *fakeMemcached) handleDelete(args []string, w *bufio.Writer) {
	if len(args) < 1 {
		_, _ = w.WriteString("CLIENT_ERROR bad command line\r\n")
		return
	}
	key := args[0]
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	if _, ok := f.store.kv[key]; !ok {
		_, _ = w.WriteString("NOT_FOUND\r\n")
		return
	}
	delete(f.store.kv, key)
	_, _ = w.WriteString("DELETED\r\n")
}

func (f *fakeMemcached) handleArith(cmd string, args []string, w *bufio.Writer) {
	if len(args) < 2 {
		_, _ = w.WriteString("CLIENT_ERROR bad command line\r\n")
		return
	}
	key := args[0]
	delta, _ := strconv.ParseUint(args[1], 10, 64)
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	it, ok := f.store.kv[key]
	if !ok {
		_, _ = w.WriteString("NOT_FOUND\r\n")
		return
	}
	cur, err := strconv.ParseUint(string(it.value), 10, 64)
	if err != nil {
		_, _ = w.WriteString("CLIENT_ERROR cannot increment or decrement non-numeric value\r\n")
		return
	}
	switch cmd {
	case "incr":
		cur += delta
	case "decr":
		if delta > cur {
			cur = 0
		} else {
			cur -= delta
		}
	}
	it.value = []byte(strconv.FormatUint(cur, 10))
	it.cas = f.store.nextCAS()
	f.store.kv[key] = it
	_, _ = w.WriteString(strconv.FormatUint(cur, 10))
	_, _ = w.WriteString("\r\n")
}

func (f *fakeMemcached) handleTouch(args []string, w *bufio.Writer) {
	if len(args) < 2 {
		_, _ = w.WriteString("CLIENT_ERROR bad command line\r\n")
		return
	}
	key := args[0]
	exp, _ := strconv.ParseInt(args[1], 10, 64)
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	it, ok := f.store.kv[key]
	if !ok {
		_, _ = w.WriteString("NOT_FOUND\r\n")
		return
	}
	it.expireAt = toAbsoluteExpiry(exp)
	f.store.kv[key] = it
	_, _ = w.WriteString("TOUCHED\r\n")
}

func (f *fakeMemcached) handleFlushAll(args []string, w *bufio.Writer) {
	// Ignore the optional delay; we flush immediately in the fake.
	_ = args
	f.store.mu.Lock()
	f.store.kv = map[string]memItem{}
	f.store.mu.Unlock()
	_, _ = w.WriteString("OK\r\n")
}

func (f *fakeMemcached) handleStats(w *bufio.Writer) {
	_, _ = w.WriteString("STAT version 1.6.celeris-fake\r\n")
	_, _ = w.WriteString("STAT pid 42\r\n")
	f.store.mu.Lock()
	n := len(f.store.kv)
	f.store.mu.Unlock()
	_, _ = w.WriteString("STAT curr_items ")
	_, _ = w.WriteString(strconv.Itoa(n))
	_, _ = w.WriteString("\r\n")
	_, _ = w.WriteString("END\r\n")
}
