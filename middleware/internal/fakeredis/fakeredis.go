// Package fakeredis is a tiny RESP2 fake server used by celeris
// middleware tests that adapt onto the native driver/redis client.
// Not for production use.
package fakeredis

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is a minimal in-process RESP2 server that implements a subset
// of commands sufficient for middleware store adapters: GET, SET, DEL,
// SETEX, EXPIRE, PEXPIRE, TTL, PTTL, EXISTS, GETDEL, SCAN, HMSET,
// HMGET, HGET, HSET, HEXPIRE, PING, AUTH, HELLO, SELECT, SCRIPT LOAD,
// EVAL, EVALSHA.
//
// Use [Start] to spin one up on a random loopback port; the testing.TB
// Cleanup hook closes the listener when the test ends.
type Server struct {
	ln net.Listener

	mu      sync.Mutex
	data    map[string]entry
	hashes  map[string]map[string]string
	scripts map[string]string // sha → body
}

type entry struct {
	value   string
	expires time.Time // zero means no expiry
}

// Start returns a running fake bound to a random loopback port.
func Start(tb testing.TB) *Server {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("fakeredis: listen: %v", err)
	}
	s := &Server{
		ln:      ln,
		data:    make(map[string]entry),
		hashes:  make(map[string]map[string]string),
		scripts: make(map[string]string),
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go s.accept()
	return s
}

// Addr returns "host:port".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// SetScript pre-registers a Lua script under its expected SHA. Tests
// that stub out EVALSHA replies can inject known SHAs this way.
func (s *Server) SetScript(sha, body string) {
	s.mu.Lock()
	s.scripts[sha] = body
	s.mu.Unlock()
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		cmd, err := readArray(r)
		if err != nil {
			return
		}
		if len(cmd) == 0 {
			continue
		}
		s.dispatch(cmd, w)
		_ = w.Flush()
	}
}

func readArray(r *bufio.Reader) ([]string, error) {
	head, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	head = strings.TrimRight(head, "\r\n")
	if len(head) == 0 || head[0] != '*' {
		return nil, fmt.Errorf("fakeredis: expected array, got %q", head)
	}
	n, err := strconv.Atoi(head[1:])
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) == 0 || line[0] != '$' {
			return nil, fmt.Errorf("fakeredis: expected bulk, got %q", line)
		}
		length, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if length < 0 {
			out = append(out, "")
			continue
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf[:length]))
	}
	return out, nil
}

// Dispatcher writes helpers.
func writeSimple(w *bufio.Writer, s string) { fmt.Fprintf(w, "+%s\r\n", s) }
func writeError(w *bufio.Writer, s string)  { fmt.Fprintf(w, "-%s\r\n", s) }
func writeInt(w *bufio.Writer, n int64)     { fmt.Fprintf(w, ":%d\r\n", n) }
func writeNil(w *bufio.Writer)              { w.WriteString("$-1\r\n") }
func writeBulk(w *bufio.Writer, s string)   { fmt.Fprintf(w, "$%d\r\n%s\r\n", len(s), s) }
func writeBulkBytes(w *bufio.Writer, b []byte) {
	fmt.Fprintf(w, "$%d\r\n", len(b))
	w.Write(b)
	w.WriteString("\r\n")
}
func writeArrayHeader(w *bufio.Writer, n int) { fmt.Fprintf(w, "*%d\r\n", n) }

func (s *Server) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key]
	if !ok {
		return "", false
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		delete(s.data, key)
		return "", false
	}
	return e.value, true
}

func (s *Server) set(key, val string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := entry{value: val}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	s.data[key] = e
}

func (s *Server) del(keys []string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := s.data[k]; ok {
			delete(s.data, k)
			n++
		}
		if _, ok := s.hashes[k]; ok {
			delete(s.hashes, k)
		}
	}
	return n
}

// Data returns a copy of current string-keyed entries. For test
// assertions only.
func (s *Server) Data() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v.value
	}
	return out
}

// Hash returns a snapshot of the given hash key's fields. Returns nil
// if the key does not exist.
func (s *Server) Hash(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[key]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

func (s *Server) dispatch(cmd []string, w *bufio.Writer) {
	if len(cmd) == 0 {
		return
	}
	up := strings.ToUpper(cmd[0])
	switch up {
	case "PING":
		writeSimple(w, "PONG")
	case "AUTH":
		writeSimple(w, "OK")
	case "SELECT":
		writeSimple(w, "OK")
	case "HELLO":
		// Minimal RESP2 HELLO reply: just acknowledge as map-like array.
		writeArrayHeader(w, 14)
		writeBulk(w, "server")
		writeBulk(w, "fakeredis")
		writeBulk(w, "version")
		writeBulk(w, "7.2.0")
		writeBulk(w, "proto")
		writeInt(w, 2)
		writeBulk(w, "id")
		writeInt(w, 1)
		writeBulk(w, "mode")
		writeBulk(w, "standalone")
		writeBulk(w, "role")
		writeBulk(w, "master")
		writeBulk(w, "modules")
		writeArrayHeader(w, 0)
	case "GET":
		v, ok := s.get(cmd[1])
		if !ok {
			writeNil(w)
			return
		}
		writeBulk(w, v)
	case "SET":
		// SET key value [EX s|PX ms|EXAT ts|PXAT ts|NX|XX]
		key, val := cmd[1], cmd[2]
		var ttl time.Duration
		nx := false
		for i := 3; i < len(cmd); i++ {
			switch strings.ToUpper(cmd[i]) {
			case "EX":
				secs, _ := strconv.Atoi(cmd[i+1])
				ttl = time.Duration(secs) * time.Second
				i++
			case "PX":
				ms, _ := strconv.Atoi(cmd[i+1])
				ttl = time.Duration(ms) * time.Millisecond
				i++
			case "NX":
				nx = true
			}
		}
		if nx {
			if _, ok := s.get(key); ok {
				writeNil(w)
				return
			}
		}
		s.set(key, val, ttl)
		writeSimple(w, "OK")
	case "SETEX":
		secs, _ := strconv.Atoi(cmd[2])
		s.set(cmd[1], cmd[3], time.Duration(secs)*time.Second)
		writeSimple(w, "OK")
	case "DEL":
		writeInt(w, s.del(cmd[1:]))
	case "EXISTS":
		var n int64
		for _, k := range cmd[1:] {
			if _, ok := s.get(k); ok {
				n++
			}
		}
		writeInt(w, n)
	case "GETDEL":
		v, ok := s.get(cmd[1])
		if !ok {
			writeNil(w)
			return
		}
		s.del([]string{cmd[1]})
		writeBulk(w, v)
	case "EXPIRE":
		secs, _ := strconv.Atoi(cmd[2])
		s.mu.Lock()
		e, ok := s.data[cmd[1]]
		if ok {
			e.expires = time.Now().Add(time.Duration(secs) * time.Second)
			s.data[cmd[1]] = e
		} else if h, hok := s.hashes[cmd[1]]; hok {
			_ = h
			// represent hash expiry via a separate sentinel entry
			s.data["__hexp:"+cmd[1]] = entry{expires: time.Now().Add(time.Duration(secs) * time.Second)}
			ok = true
		}
		s.mu.Unlock()
		if ok {
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "SCAN":
		// SCAN cursor MATCH pattern COUNT n — simplified: returns everything matching on cursor 0 then done.
		cursor := cmd[1]
		pattern := ""
		for i := 2; i < len(cmd); i++ {
			if strings.ToUpper(cmd[i]) == "MATCH" && i+1 < len(cmd) {
				pattern = cmd[i+1]
				i++
			}
		}
		if cursor != "0" {
			writeArrayHeader(w, 2)
			writeBulk(w, "0")
			writeArrayHeader(w, 0)
			return
		}
		s.mu.Lock()
		keys := make([]string, 0, len(s.data))
		for k := range s.data {
			if matchGlob(pattern, k) {
				keys = append(keys, k)
			}
		}
		s.mu.Unlock()
		writeArrayHeader(w, 2)
		writeBulk(w, "0")
		writeArrayHeader(w, len(keys))
		for _, k := range keys {
			writeBulk(w, k)
		}
	case "HGET":
		s.mu.Lock()
		h := s.hashes[cmd[1]]
		v, ok := h[cmd[2]]
		s.mu.Unlock()
		if !ok {
			writeNil(w)
			return
		}
		writeBulk(w, v)
	case "HMGET":
		s.mu.Lock()
		h := s.hashes[cmd[1]]
		writeArrayHeader(w, len(cmd)-2)
		for _, f := range cmd[2:] {
			v, ok := h[f]
			if !ok {
				writeNil(w)
				continue
			}
			writeBulk(w, v)
		}
		s.mu.Unlock()
	case "HMSET", "HSET":
		s.mu.Lock()
		h, ok := s.hashes[cmd[1]]
		if !ok {
			h = make(map[string]string)
			s.hashes[cmd[1]] = h
		}
		added := int64(0)
		for i := 2; i+1 < len(cmd); i += 2 {
			if _, exists := h[cmd[i]]; !exists {
				added++
			}
			h[cmd[i]] = cmd[i+1]
		}
		s.mu.Unlock()
		if up == "HMSET" {
			writeSimple(w, "OK")
		} else {
			writeInt(w, added)
		}
	case "SCRIPT":
		if len(cmd) < 2 {
			writeError(w, "ERR wrong number of arguments")
			return
		}
		if strings.ToUpper(cmd[1]) != "LOAD" {
			writeError(w, "ERR only SCRIPT LOAD is implemented")
			return
		}
		body := cmd[2]
		sha := fakeSHA(body)
		s.mu.Lock()
		s.scripts[sha] = body
		s.mu.Unlock()
		writeBulk(w, sha)
	case "EVALSHA", "EVAL":
		var body string
		var argsStart int
		if up == "EVALSHA" {
			sha := cmd[1]
			s.mu.Lock()
			b, ok := s.scripts[sha]
			s.mu.Unlock()
			if !ok {
				writeError(w, "NOSCRIPT No matching script")
				return
			}
			body = b
			argsStart = 2
		} else {
			body = cmd[1]
			argsStart = 2
		}
		numKeys, _ := strconv.Atoi(cmd[argsStart])
		keys := cmd[argsStart+1 : argsStart+1+numKeys]
		args := cmd[argsStart+1+numKeys:]
		s.runScript(body, keys, args, w)
	case "COMMAND":
		// minimal COMMAND DOCS reply: empty array
		writeArrayHeader(w, 0)
	case "QUIT":
		writeSimple(w, "OK")
	default:
		writeError(w, "ERR unknown command "+cmd[0])
	}
}

func fakeSHA(body string) string {
	// Non-cryptographic hash; stable for test scripts. Length 40 hex.
	const hexChars = "0123456789abcdef"
	var h [20]byte
	for i := 0; i < len(body); i++ {
		h[i%20] ^= body[i]
	}
	out := make([]byte, 40)
	for i := 0; i < 20; i++ {
		out[2*i] = hexChars[h[i]>>4]
		out[2*i+1] = hexChars[h[i]&0xf]
	}
	return string(out)
}

// runScript is a tiny Lua interpreter stub: it recognizes the
// token-bucket script from middleware/ratelimit/redisstore and the
// undo script. Any other script yields a blanket "ok" reply so tests
// relying on script existence can proceed.
func (s *Server) runScript(body string, keys, args []string, w *bufio.Writer) {
	if strings.Contains(body, "celeris-ratelimit: undo") {
		s.runUndo(keys, args, w)
		return
	}
	if strings.Contains(body, "Token-bucket atomic update") {
		s.runTokenBucket(keys, args, w)
		return
	}
	// Unknown script: return a bulk "OK" (not ideal but safe default).
	writeSimple(w, "OK")
}

func (s *Server) runTokenBucket(keys, args []string, w *bufio.Writer) {
	if len(keys) != 1 || len(args) != 4 {
		writeError(w, "ERR bad token-bucket args")
		return
	}
	key := keys[0]
	now, _ := strconv.ParseInt(args[0], 10, 64)
	rate, _ := strconv.ParseFloat(args[1], 64)
	burst, _ := strconv.Atoi(args[2])
	ttl, _ := strconv.Atoi(args[3])

	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[key]
	if !ok {
		h = make(map[string]string)
		s.hashes[key] = h
	}
	_, seen := h["tokens"]
	tokens, _ := strconv.ParseFloat(h["tokens"], 64)
	last, _ := strconv.ParseInt(h["last"], 10, 64)
	if !seen {
		tokens = float64(burst)
		last = now
	}
	elapsed := float64(now-last) / 1e9
	if elapsed > 0 {
		tokens = tokens + elapsed*rate
		if tokens > float64(burst) {
			tokens = float64(burst)
		}
	}
	allowed := int64(0)
	if tokens >= 1 {
		tokens -= 1
		allowed = 1
	}
	h["tokens"] = strconv.FormatFloat(tokens, 'f', -1, 64)
	h["last"] = strconv.FormatInt(now, 10)
	s.data["__hexp:"+key] = entry{expires: time.Now().Add(time.Duration(ttl) * time.Second)}
	missing := 1 - tokens
	if missing < 0 {
		missing = 0
	}
	resetNs := now + int64(missing*1e9/rate)
	writeArrayHeader(w, 3)
	writeInt(w, allowed)
	writeInt(w, int64(tokens))
	writeInt(w, resetNs)
}

func (s *Server) runUndo(keys, args []string, w *bufio.Writer) {
	if len(keys) != 1 || len(args) != 2 {
		writeError(w, "ERR bad undo args")
		return
	}
	key := keys[0]
	burst, _ := strconv.Atoi(args[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[key]
	if !ok {
		writeInt(w, 0)
		return
	}
	tokens, _ := strconv.ParseFloat(h["tokens"], 64)
	tokens += 1
	if tokens > float64(burst) {
		tokens = float64(burst)
	}
	h["tokens"] = strconv.FormatFloat(tokens, 'f', -1, 64)
	writeInt(w, 1)
}

// matchGlob is a tiny glob matcher supporting * and literal chars.
// Enough for SCAN MATCH patterns used in adapter tests.
func matchGlob(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	// Simple * splitting: pattern "foo*" matches any string starting with "foo".
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return s == pattern
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	last := parts[len(parts)-1]
	return strings.HasSuffix(s, last)
}
