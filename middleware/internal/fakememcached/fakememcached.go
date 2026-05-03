// Package fakememcached is a tiny text-protocol fake memcached server
// for middleware store-adapter tests. Not for production use.
//
// Implemented commands: get, gets, set, add, delete, cas, version,
// quit. Missing: replace, append/prepend, incr/decr, flush_all — not
// used by any of the bundled middleware-store adapters.
package fakememcached

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Server is an in-process fake memcached text-protocol server bound
// to a random loopback port.
type Server struct {
	ln      net.Listener
	mu      sync.Mutex
	data    map[string]*item
	nextCAS atomic.Uint64
}

type item struct {
	value  []byte
	flags  uint32
	expire time.Time // zero = no expiry
	cas    uint64
}

// Start spins up a fake memcached on a random loopback port. The
// listener is closed via t.Cleanup.
func Start(tb testing.TB) *Server {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("fakememcached: listen: %v", err)
	}
	s := &Server{ln: ln, data: make(map[string]*item)}
	tb.Cleanup(func() { _ = ln.Close() })
	go s.accept()
	return s
}

// Addr returns "host:port".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Data returns a copy of current key→value entries (ignoring expiry).
func (s *Server) Data() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = string(v.value)
	}
	return out
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
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		cmd := parts[0]
		switch cmd {
		case "get":
			s.handleGet(parts[1:], w, false)
		case "gets":
			s.handleGet(parts[1:], w, true)
		case "set", "add":
			s.handleStore(cmd, parts[1:], r, w, 0)
		case "cas":
			s.handleCAS(parts[1:], r, w)
		case "delete":
			s.handleDelete(parts[1:], w)
		case "version":
			_, _ = w.WriteString("VERSION celeris-fakememcached-0.1\r\n")
		case "quit":
			return
		default:
			_, _ = w.WriteString("ERROR\r\n")
		}
		_ = w.Flush()
	}
}

func (s *Server) handleGet(keys []string, w *bufio.Writer, withCAS bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		it, ok := s.data[k]
		if !ok {
			continue
		}
		if !it.expire.IsZero() && time.Now().After(it.expire) {
			delete(s.data, k)
			continue
		}
		if withCAS {
			_, _ = fmt.Fprintf(w, "VALUE %s %d %d %d\r\n", k, it.flags, len(it.value), it.cas)
		} else {
			_, _ = fmt.Fprintf(w, "VALUE %s %d %d\r\n", k, it.flags, len(it.value))
		}
		_, _ = w.Write(it.value)
		_, _ = w.WriteString("\r\n")
	}
	_, _ = w.WriteString("END\r\n")
}

func (s *Server) handleStore(cmd string, args []string, r *bufio.Reader, w *bufio.Writer, _ uint64) {
	// args: <key> <flags> <exptime> <bytes> [noreply]
	if len(args) < 4 {
		_, _ = w.WriteString("CLIENT_ERROR bad args\r\n")
		return
	}
	key := args[0]
	flags, _ := strconv.ParseUint(args[1], 10, 32)
	exp, _ := strconv.ParseInt(args[2], 10, 64)
	size, _ := strconv.Atoi(args[3])
	body := make([]byte, size+2)
	if _, err := io.ReadFull(r, body); err != nil {
		_, _ = w.WriteString("CLIENT_ERROR short data\r\n")
		return
	}
	body = body[:size]
	expireAt := expTime(exp)

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, has := s.data[key]
	if cmd == "add" && has && (existing.expire.IsZero() || existing.expire.After(time.Now())) {
		_, _ = w.WriteString("NOT_STORED\r\n")
		return
	}
	cas := s.nextCAS.Add(1)
	s.data[key] = &item{value: append([]byte(nil), body...), flags: uint32(flags), expire: expireAt, cas: cas}
	_, _ = w.WriteString("STORED\r\n")
}

func (s *Server) handleCAS(args []string, r *bufio.Reader, w *bufio.Writer) {
	// args: <key> <flags> <exptime> <bytes> <cas-unique> [noreply]
	if len(args) < 5 {
		_, _ = w.WriteString("CLIENT_ERROR bad args\r\n")
		return
	}
	key := args[0]
	flags, _ := strconv.ParseUint(args[1], 10, 32)
	exp, _ := strconv.ParseInt(args[2], 10, 64)
	size, _ := strconv.Atoi(args[3])
	casID, _ := strconv.ParseUint(args[4], 10, 64)
	body := make([]byte, size+2)
	if _, err := io.ReadFull(r, body); err != nil {
		_, _ = w.WriteString("CLIENT_ERROR short data\r\n")
		return
	}
	body = body[:size]

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, has := s.data[key]
	if !has {
		_, _ = w.WriteString("NOT_FOUND\r\n")
		return
	}
	if existing.cas != casID {
		_, _ = w.WriteString("EXISTS\r\n")
		return
	}
	cas := s.nextCAS.Add(1)
	s.data[key] = &item{value: append([]byte(nil), body...), flags: uint32(flags), expire: expTime(exp), cas: cas}
	_, _ = w.WriteString("STORED\r\n")
}

func (s *Server) handleDelete(args []string, w *bufio.Writer) {
	if len(args) < 1 {
		_, _ = w.WriteString("CLIENT_ERROR bad args\r\n")
		return
	}
	key := args[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		_, _ = w.WriteString("NOT_FOUND\r\n")
		return
	}
	delete(s.data, key)
	_, _ = w.WriteString("DELETED\r\n")
}

func expTime(exp int64) time.Time {
	if exp == 0 {
		return time.Time{}
	}
	if exp <= 2592000 {
		return time.Now().Add(time.Duration(exp) * time.Second)
	}
	return time.Unix(exp, 0)
}
