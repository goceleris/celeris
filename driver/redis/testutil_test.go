package redis

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// fakeRedis is a minimal in-process RESP server used by tests. It accepts
// client connections, parses RESP2 command arrays, and feeds them to a
// scripted handler function. The handler writes raw RESP bytes back via
// the writer argument.
type fakeRedis struct {
	ln       net.Listener
	handler  func(cmd []string, w *bufio.Writer)
	mu       sync.Mutex
	conns    []net.Conn
	writerMu map[*bufio.Writer]*sync.Mutex
}

// WriterMutex returns (and lazily creates) a mutex that must be held by
// background writers (e.g., a pubsub publisher goroutine) when writing to w.
// The serve goroutine holds this mutex around every handler invocation + flush.
func (f *fakeRedis) WriterMutex(w *bufio.Writer) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writerMu == nil {
		f.writerMu = map[*bufio.Writer]*sync.Mutex{}
	}
	m, ok := f.writerMu[w]
	if !ok {
		m = &sync.Mutex{}
		f.writerMu[w] = m
	}
	return m
}

// startFakeRedis returns a running fake server bound to a random port.
func startFakeRedis(t *testing.T, handler func(cmd []string, w *bufio.Writer)) *fakeRedis {
	t.Helper()
	return startFakeRedisTB(t, handler)
}

// startFakeRedisBench is the benchmark variant accepting *testing.B.
func startFakeRedisBench(b *testing.B, handler func(cmd []string, w *bufio.Writer)) *fakeRedis {
	b.Helper()
	return startFakeRedisTB(b, handler)
}

func startFakeRedisTB(tb testing.TB, handler func(cmd []string, w *bufio.Writer)) *fakeRedis {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	f := &fakeRedis{ln: ln, handler: handler}
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
func (f *fakeRedis) Addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) accept() {
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

func (f *fakeRedis) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	wm := f.WriterMutex(w)
	var argsBuf []string
	for {
		cmd, err := readCommandInto(r, argsBuf[:0])
		if err != nil {
			return
		}
		argsBuf = cmd
		wm.Lock()
		f.handler(cmd, w)
		err = w.Flush()
		wm.Unlock()
		if err != nil {
			return
		}
	}
}

// readCommand parses a RESP2 array of bulk strings from r. Uses ReadSlice
// (not ReadString) so the per-line buffer is not heap-allocated by bufio's
// strings.Builder path — this keeps the fake server's steady-state loop
// from polluting the driver's allocation measurements in TestAllocBudgets.
func readCommand(r *bufio.Reader) ([]string, error) {
	return readCommandInto(r, nil)
}

// internCmd short-circuits the string allocation for common command bytes.
// TestAllocBudgets measures server-side allocs too (same goroutine path);
// pipelines that hammer the same command (e.g. INCR ctr, GET k) otherwise
// pay one alloc per arg per command.
func internCmd(b []byte) (string, bool) {
	switch len(b) {
	case 1:
		if b[0] == 'k' {
			return "k", true
		}
	case 3:
		switch string(b) {
		case "GET":
			return "GET", true
		case "SET":
			return "SET", true
		case "DEL":
			return "DEL", true
		case "ctr":
			return "ctr", true
		}
	case 4:
		switch string(b) {
		case "INCR":
			return "INCR", true
		case "DECR":
			return "DECR", true
		case "HGET":
			return "HGET", true
		case "HSET":
			return "HSET", true
		case "MGET":
			return "MGET", true
		case "MSET":
			return "MSET", true
		case "PING":
			return "PING", true
		case "EXEC":
			return "EXEC", true
		}
	case 5:
		switch string(b) {
		case "HELLO":
			return "HELLO", true
		case "MULTI":
			return "MULTI", true
		case "RPUSH":
			return "RPUSH", true
		case "LPUSH":
			return "LPUSH", true
		case "LPOP ":
			return "LPOP", true
		}
	case 6:
		switch string(b) {
		case "EXISTS":
			return "EXISTS", true
		case "EXPIRE":
			return "EXPIRE", true
		case "SELECT":
			return "SELECT", true
		case "WATCH ":
			return "WATCH", true
		}
	}
	return "", false
}

// readCommandInto is the pooled variant; it reuses dst's backing array for
// the arg slice when dst has enough capacity.
func readCommandInto(r *bufio.Reader, dst []string) ([]string, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("fake: expected array, got %q", line)
	}
	n, err := atoiBytes(line[1 : len(line)-2])
	if err != nil {
		return nil, err
	}
	var args []string
	if cap(dst) >= n {
		args = dst[:n]
	} else {
		args = make([]string, n)
	}
	for i := 0; i < n; i++ {
		lenLine, err := r.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		if lenLine[0] != '$' {
			return nil, fmt.Errorf("fake: expected bulk, got %q", lenLine)
		}
		blen, err := atoiBytes(lenLine[1 : len(lenLine)-2])
		if err != nil {
			return nil, err
		}
		// ReadSlice returns a slice aliasing bufio's buffer; it's valid until
		// the next read. Peek the payload + \r\n, copy to a fresh string,
		// then Discard. This collapses two allocs (buf + string(buf)) into
		// one (the string).
		peeked, err := r.Peek(blen + 2)
		if err != nil {
			if err == bufio.ErrBufferFull {
				// fallback to the safe path for oversized payloads.
				buf := make([]byte, blen)
				if _, err := io.ReadFull(r, buf); err != nil {
					return nil, err
				}
				if _, err := r.Discard(2); err != nil {
					return nil, err
				}
				if s, ok := internCmd(buf); ok {
					args[i] = s
				} else {
					args[i] = string(buf)
				}
				continue
			}
			return nil, err
		}
		if s, ok := internCmd(peeked[:blen]); ok {
			args[i] = s
		} else {
			args[i] = string(peeked[:blen])
		}
		if _, err := r.Discard(blen + 2); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func atoiBytes(b []byte) (int, error) {
	n := 0
	neg := false
	if len(b) > 0 && b[0] == '-' {
		neg = true
		b = b[1:]
	}
	if len(b) == 0 {
		return 0, fmt.Errorf("fake: empty number")
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("fake: bad digit %q", c)
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ---- helper response writers ----

func writeSimple(w *bufio.Writer, s string) {
	_ = w.WriteByte('+')
	_, _ = w.WriteString(s)
	_, _ = w.WriteString("\r\n")
}

func writeError(w *bufio.Writer, s string) {
	_ = w.WriteByte('-')
	_, _ = w.WriteString(s)
	_, _ = w.WriteString("\r\n")
}

func writeInt(w *bufio.Writer, n int64) {
	// Format directly into bufio's internal buffer through AvailableBuffer,
	// sidestepping any heap allocation that strconv.FormatInt would incur.
	// bufio.Writer exposes its underlying byte slice for exactly this use;
	// the returned slice aliases the internal buffer and the matching
	// Write() call advances w.n to include our appended bytes.
	b := w.AvailableBuffer()
	b = append(b, ':')
	b = strconv.AppendInt(b, n, 10)
	b = append(b, '\r', '\n')
	_, _ = w.Write(b)
}

func writeBulk(w *bufio.Writer, s string) {
	_ = w.WriteByte('$')
	_, _ = w.WriteString(strconv.Itoa(len(s)))
	_, _ = w.WriteString("\r\n")
	_, _ = w.WriteString(s)
	_, _ = w.WriteString("\r\n")
}

func writeNullBulk(w *bufio.Writer) {
	_, _ = w.WriteString("$-1\r\n")
}

func writeArrayHeader(w *bufio.Writer, n int) {
	_ = w.WriteByte('*')
	_, _ = w.WriteString(strconv.Itoa(n))
	_, _ = w.WriteString("\r\n")
}

func writePush(w *bufio.Writer, items ...string) {
	_ = w.WriteByte('>')
	_, _ = w.WriteString(strconv.Itoa(len(items)))
	_, _ = w.WriteString("\r\n")
	for _, i := range items {
		writeBulk(w, i)
	}
}

func writeArrayBulks(w *bufio.Writer, items ...string) {
	writeArrayHeader(w, len(items))
	for _, i := range items {
		writeBulk(w, i)
	}
}

// splitHostPort wraps net.SplitHostPort for test convenience.
func splitHostPort(addr string) (string, string, error) {
	return net.SplitHostPort(addr)
}

// handleCommonHELLO answers HELLO with a minimal RESP2 map (encoded as a
// RESP2 array of alternating key/value bulks, since RESP3 HELLO maps are the
// "clients speak RESP3 after this" signal). For tests that don't care about
// proto details we just write +OK-like replies here.
func handleHELLO(w *bufio.Writer, proto int) {
	// Write a RESP2 array of KV pairs with minimal fields.
	writeArrayHeader(w, 6)
	writeBulk(w, "server")
	writeBulk(w, "fake")
	writeBulk(w, "version")
	writeBulk(w, "7.0.0")
	writeBulk(w, "proto")
	writeInt(w, int64(proto))
}
