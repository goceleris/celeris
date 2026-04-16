// Package integration_test verifies the headline v1.4.0 architecture: a
// celeris HTTP handler issuing database and cache I/O via the shared event
// loop exposed by the celeris.Server.
//
// This test is darwin-compatible (no linux build tag). The std engine does
// not implement [engine.EventLoopProvider], so [celeris.Server.EventLoopProvider]
// returns nil and the drivers fall back to the package-level standalone mini
// event loop resolved via [eventloop.Resolve]. The darwin standalone loop
// uses net.FileConn + a reader goroutine (driver/internal/eventloop/loop_other.go),
// so this still exercises the end-to-end driver→server→HTTP response path.
//
// On Linux with one of the native engines (epoll, io_uring, adaptive) these
// same tests would exercise the server-provided provider — the drivers'
// WithEngine option picks up the provider through the exact code path this
// test covers.
package integration_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
	pgproto "github.com/goceleris/celeris/driver/postgres/protocol"
	"github.com/goceleris/celeris/driver/redis"
)

// -----------------------------------------------------------------------------
// Fake Postgres server — minimal subset lifted from driver/postgres/conn_test.go.
// Duplicated here because the test-package fakes are unexported. Supports the
// trust-auth startup exchange plus a single SELECT that replies with one row.
// -----------------------------------------------------------------------------

type fakePG struct {
	ln      net.Listener
	handler func(net.Conn)
}

func startFakePG(t *testing.T, handler func(net.Conn)) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake pg listen: %v", err)
	}
	f := &fakePG{ln: ln, handler: handler}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return f
}

func (f *fakePG) Addr() string { return f.ln.Addr().String() }

func pgReadStartup(c net.Conn) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c, lenBuf); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint32(lenBuf))
	if n < 4 {
		return io.ErrUnexpectedEOF
	}
	body := make([]byte, n-4)
	_, err := io.ReadFull(c, body)
	return err
}

func pgWriteMsg(c net.Conn, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := c.Write(buf)
	return err
}

func pgReadMsg(c net.Conn) (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return 0, nil, err
	}
	typ := hdr[0]
	n := int(binary.BigEndian.Uint32(hdr[1:5]))
	if n < 4 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

func pgBuildRowDescription(name string, oid uint32, size int16) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out[0:2], 1)
	out = append(out, []byte(name)...)
	out = append(out, 0)
	var buf [18]byte
	binary.BigEndian.PutUint32(buf[0:4], 0)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint32(buf[6:10], oid)
	binary.BigEndian.PutUint16(buf[10:12], uint16(size))
	binary.BigEndian.PutUint32(buf[12:16], 0xFFFFFFFF)
	binary.BigEndian.PutUint16(buf[16:18], 0) // text format
	out = append(out, buf[:]...)
	return out
}

func pgBuildDataRow(field []byte) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out[0:2], 1)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(field)))
	out = append(out, lenBuf...)
	out = append(out, field...)
	return out
}

// pgSelect1Handler accepts connections, drives trust-auth, then for every
// Query message replies with a single-row RowDescription+DataRow for value=1.
// Handles arbitrarily many queries on the same connection (pool may reuse).
func pgSelect1Handler(c net.Conn) {
	defer c.Close()
	if err := pgReadStartup(c); err != nil {
		return
	}
	// AuthenticationOk ('R' with int32=0).
	if err := pgWriteMsg(c, pgproto.BackendAuthentication, make([]byte, 4)); err != nil {
		return
	}
	// BackendKeyData.
	key := make([]byte, 8)
	binary.BigEndian.PutUint32(key[0:4], 1)
	binary.BigEndian.PutUint32(key[4:8], 2)
	if err := pgWriteMsg(c, pgproto.BackendBackendKeyData, key); err != nil {
		return
	}
	if err := pgWriteMsg(c, pgproto.BackendReadyForQuery, []byte{'I'}); err != nil {
		return
	}
	for {
		typ, _, err := pgReadMsg(c)
		if err != nil {
			return
		}
		if typ == pgproto.MsgTerminate {
			return
		}
		if typ != pgproto.MsgQuery {
			continue
		}
		rd := pgBuildRowDescription("n", pgproto.OIDInt4, 4)
		if err := pgWriteMsg(c, pgproto.BackendRowDescription, rd); err != nil {
			return
		}
		if err := pgWriteMsg(c, pgproto.BackendDataRow, pgBuildDataRow([]byte("1"))); err != nil {
			return
		}
		// CommandComplete 'C' with "SELECT 1\0".
		tag := append([]byte("SELECT 1"), 0)
		if err := pgWriteMsg(c, pgproto.BackendCommandComplete, tag); err != nil {
			return
		}
		if err := pgWriteMsg(c, pgproto.BackendReadyForQuery, []byte{'I'}); err != nil {
			return
		}
	}
}

// -----------------------------------------------------------------------------
// Fake Redis — minimal RESP2 responder lifted from driver/redis/testutil_test.go.
// -----------------------------------------------------------------------------

type fakeRedis struct {
	ln      net.Listener
	handler func(cmd []string, w *bufio.Writer)
	mu      sync.Mutex
	conns   []net.Conn
}

func startFakeRedis(t *testing.T, handler func(cmd []string, w *bufio.Writer)) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake redis listen: %v", err)
	}
	f := &fakeRedis{ln: ln, handler: handler}
	t.Cleanup(func() {
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
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		cmd, err := readRESP(r)
		if err != nil {
			return
		}
		f.handler(cmd, w)
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func readRESP(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("fake: expected array, got %q", line)
	}
	n, err := strconv.Atoi(strings.TrimRight(line[1:], "\r\n"))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := 0; i < n; i++ {
		lenLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if lenLine[0] != '$' {
			return nil, fmt.Errorf("fake: expected bulk, got %q", lenLine)
		}
		blen, err := strconv.Atoi(strings.TrimRight(lenLine[1:], "\r\n"))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, blen)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if _, err := r.Discard(2); err != nil {
			return nil, err
		}
		args[i] = string(buf)
	}
	return args, nil
}

func respSimple(w *bufio.Writer, s string) { _, _ = w.WriteString("+" + s + "\r\n") }
func respBulk(w *bufio.Writer, s string) {
	_, _ = w.WriteString("$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n")
}
func respInt(w *bufio.Writer, n int64) {
	_, _ = w.WriteString(":" + strconv.FormatInt(n, 10) + "\r\n")
}
func respArray(w *bufio.Writer, items ...string) {
	_, _ = w.WriteString("*" + strconv.Itoa(len(items)) + "\r\n")
	for _, it := range items {
		respBulk(w, it)
	}
}

// redisKVHandler returns a handler that answers HELLO with a RESP2 map, PING
// with +PONG, GET k with "value", and SET with +OK. Good enough for the test.
func redisKVHandler() func([]string, *bufio.Writer) {
	return func(cmd []string, w *bufio.Writer) {
		if len(cmd) == 0 {
			respSimple(w, "OK")
			return
		}
		switch strings.ToUpper(cmd[0]) {
		case "HELLO":
			// Minimal RESP2 array "map" (server, fake, version, 7.0.0, proto, 2).
			// The driver negotiates RESP2 or RESP3 but accepts this canned map.
			_, _ = w.WriteString("*6\r\n")
			respBulk(w, "server")
			respBulk(w, "fake")
			respBulk(w, "version")
			respBulk(w, "7.0.0")
			respBulk(w, "proto")
			respInt(w, 2)
		case "PING":
			respSimple(w, "PONG")
		case "GET":
			if len(cmd) >= 2 && cmd[1] == "k" {
				respBulk(w, "value")
			} else {
				// RESP2 null bulk.
				_, _ = w.WriteString("$-1\r\n")
			}
		case "SET":
			respSimple(w, "OK")
		case "SELECT", "AUTH", "CLIENT":
			respSimple(w, "OK")
		default:
			respArray(w)
		}
	}
}

// -----------------------------------------------------------------------------
// Server helpers.
// -----------------------------------------------------------------------------

// startServer starts a celeris.Server backed by the std engine on a free port
// and returns the bound base URL. The server is shut down on test cleanup.
func startServer(t *testing.T, register func(*celeris.Server)) string {
	t.Helper()
	srv := celeris.New(celeris.Config{
		Addr:   "127.0.0.1:0",
		Engine: celeris.Std,
	})
	register(srv)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()

	deadline := time.Now().Add(3 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server exited early: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatal("server did not bind within deadline")
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	// Document which provider path is exercised, for test log transparency.
	if srv.EventLoopProvider() == nil {
		t.Logf("server EventLoopProvider() is nil (std engine) — drivers use the standalone fallback loop")
	} else {
		t.Logf("server EventLoopProvider() is non-nil — drivers share the HTTP engine's workers")
	}

	return "http://" + srv.Addr().String()
}

// -----------------------------------------------------------------------------
// Tests.
// -----------------------------------------------------------------------------

// TestEndToEndPostgresViaServer proves a celeris HTTP handler can open a
// postgres.Pool scoped to the running server and service a SELECT inside the
// request handler, returning the DB result as the HTTP response body.
func TestEndToEndPostgresViaServer(t *testing.T) {
	pg := startFakePG(t, pgSelect1Handler)
	host, port, _ := net.SplitHostPort(pg.Addr())

	var pool *postgres.Pool
	base := startServer(t, func(srv *celeris.Server) {
		dsn := fmt.Sprintf("postgres://u@%s:%s/d?sslmode=disable", host, port)
		p, err := postgres.Open(
			dsn,
			postgres.WithEngine(srv),
			postgres.WithMaxOpen(2),
			postgres.WithStatementCacheSize(4),
		)
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		pool = p

		srv.GET("/db", func(c *celeris.Context) error {
			ctx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
			defer cancel()
			rows, err := pool.QueryContext(ctx, "SELECT 1")
			if err != nil {
				return c.String(http.StatusInternalServerError, "query error: %v", err)
			}
			defer rows.Close()
			if cols := rows.Columns(); len(cols) != 1 {
				return c.String(http.StatusInternalServerError, "unexpected cols: %v", cols)
			}
			if !rows.Next() {
				return c.String(http.StatusInternalServerError, "no row")
			}
			var val any
			if err := rows.Scan(&val); err != nil {
				return c.String(http.StatusInternalServerError, "scan error: %v", err)
			}
			return c.String(http.StatusOK, "%v", val)
		})
	})
	t.Cleanup(func() { _ = pool.Close() })

	resp, err := http.Get(base + "/db")
	if err != nil {
		t.Fatalf("GET /db: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// pgConn.simpleQuery returns text-format fields as []byte; Rows.Next
	// wraps []byte back into dest[0]. String formatting prints the byte slice.
	got := strings.TrimSpace(string(body))
	if got != "1" && got != "[49]" {
		t.Errorf("body = %q, want \"1\"", got)
	}
}

// TestEndToEndRedisViaServer proves a celeris HTTP handler can open a redis
// Client scoped to the running server and service a GET inside the handler.
func TestEndToEndRedisViaServer(t *testing.T) {
	rd := startFakeRedis(t, redisKVHandler())

	var client *redis.Client
	base := startServer(t, func(srv *celeris.Server) {
		c, err := redis.NewClient(rd.Addr(), redis.WithEngine(srv), redis.WithForceRESP2())
		if err != nil {
			t.Fatalf("redis.NewClient: %v", err)
		}
		client = c

		srv.GET("/cache", func(ctx *celeris.Context) error {
			rctx, cancel := context.WithTimeout(ctx.Context(), 3*time.Second)
			defer cancel()
			v, err := client.DoString(rctx, "GET", "k")
			if err != nil {
				return ctx.String(http.StatusInternalServerError, "get error: %v", err)
			}
			return ctx.String(http.StatusOK, "%s", v)
		})
	})
	t.Cleanup(func() { _ = client.Close() })

	resp, err := http.Get(base + "/cache")
	if err != nil {
		t.Fatalf("GET /cache: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "value" {
		t.Errorf("body = %q, want \"value\"", got)
	}
}

// TestEndToEndBothDrivers drives a single handler that touches both Postgres
// and Redis in sequence via the same server, proving the two drivers coexist
// on the shared (or standalone-fallback) event loop.
func TestEndToEndBothDrivers(t *testing.T) {
	pg := startFakePG(t, pgSelect1Handler)
	rd := startFakeRedis(t, redisKVHandler())
	host, port, _ := net.SplitHostPort(pg.Addr())

	var pool *postgres.Pool
	var client *redis.Client
	base := startServer(t, func(srv *celeris.Server) {
		dsn := fmt.Sprintf("postgres://u@%s:%s/d?sslmode=disable", host, port)
		p, err := postgres.Open(dsn, postgres.WithEngine(srv), postgres.WithMaxOpen(2))
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		pool = p
		rc, err := redis.NewClient(rd.Addr(), redis.WithEngine(srv), redis.WithForceRESP2())
		if err != nil {
			t.Fatalf("redis.NewClient: %v", err)
		}
		client = rc

		srv.GET("/combo", func(c *celeris.Context) error {
			ctx, cancel := context.WithTimeout(c.Context(), 3*time.Second)
			defer cancel()

			rows, err := pool.QueryContext(ctx, "SELECT 1")
			if err != nil {
				return c.String(http.StatusInternalServerError, "pg: %v", err)
			}
			if !rows.Next() {
				rows.Close()
				return c.String(http.StatusInternalServerError, "pg: no row")
			}
			rows.Close()

			v, err := client.DoString(ctx, "GET", "k")
			if err != nil {
				return c.String(http.StatusInternalServerError, "redis: %v", err)
			}
			return c.String(http.StatusOK, "redis=%s", v)
		})
	})
	t.Cleanup(func() {
		_ = pool.Close()
		_ = client.Close()
	})

	resp, err := http.Get(base + "/combo")
	if err != nil {
		t.Fatalf("GET /combo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "redis=value" {
		t.Errorf("body = %q, want \"redis=value\"", got)
	}
}
