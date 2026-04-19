package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

func TestEncodeTextRow(t *testing.T) {
	cases := []struct {
		name string
		in   []any
		want string
	}{
		{"nil_is_N", []any{nil}, `\N` + "\n"},
		{"bool", []any{true, false}, "t\tf\n"},
		{"int", []any{int64(42), int32(-1)}, "42\t-1\n"},
		{"float", []any{3.14}, "3.14\n"},
		{"string_plain", []any{"hello"}, "hello\n"},
		{"string_escape", []any{"a\tb\nc\\d"}, `a\tb\nc\\d` + "\n"},
		{"bytes", []any{[]byte{'x', '\t', 'y'}}, `x\ty` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(encodeTextRow(nil, tc.in))
			if got != tc.want {
				t.Fatalf("encodeTextRow(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCopyFromBasic(t *testing.T) {
	const wantRows = 100

	// Fake server scripts a COPY FROM STDIN exchange: Query -> CopyInResponse,
	// then consumes CopyData frames until CopyDone, then CommandComplete + RFQ.
	var receivedMu sync.Mutex
	var received [][]byte
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			typ, body, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			if !bytes.Contains(body, []byte("COPY")) {
				t.Errorf("expected COPY query, got %q", string(body))
				return
			}
			// CopyInResponse: format=0 (text), 2 columns, text formats.
			resp := []byte{0, 0, 2, 0, 0, 0, 0}
			if err := writeMsg(c, protocol.BackendCopyInResponse, resp); err != nil {
				return
			}
			for {
				typ, payload, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgCopyData {
					cp := make([]byte, len(payload))
					copy(cp, payload)
					receivedMu.Lock()
					received = append(received, cp)
					receivedMu.Unlock()
					continue
				}
				if typ == protocol.MsgCopyDone {
					break
				}
				if typ == protocol.MsgTerminate {
					return
				}
			}
			_ = writeCommandComplete(c, fmt.Sprintf("COPY %d", wantRows))
			_ = writeReadyForQuery(c, 'I')
			_, _ = io.Copy(io.Discard, c)
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	rows := make([][]any, wantRows)
	for i := range rows {
		rows[i] = []any{int64(i), fmt.Sprintf("row%d", i)}
	}
	n, err := conn.copyFrom(ctx, "t", []string{"id", "name"}, CopyFromSlice(rows))
	if err != nil {
		t.Fatalf("copyFrom: %v", err)
	}
	if n != int64(wantRows) {
		t.Errorf("got %d rows imported, want %d", n, wantRows)
	}
	receivedMu.Lock()
	gotReceived := len(received)
	var firstRow []byte
	if gotReceived > 0 {
		firstRow = append([]byte(nil), received[0]...)
	}
	receivedMu.Unlock()
	if gotReceived != wantRows {
		t.Errorf("server saw %d CopyData frames, want %d", gotReceived, wantRows)
	}
	// Spot-check content: first row should be "0\trow0\n".
	if firstRow != nil && string(firstRow) != "0\trow0\n" {
		t.Errorf("first row = %q, want %q", firstRow, "0\trow0\n")
	}
}

func TestCopyToBasic(t *testing.T) {
	const wantRows = 100
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			typ, _, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			// CopyOutResponse: format=0, 1 column, text.
			resp := []byte{0, 0, 1, 0, 0}
			_ = writeMsg(c, protocol.BackendCopyOutResponse, resp)
			for i := 0; i < wantRows; i++ {
				payload := []byte(fmt.Sprintf("row%d\n", i))
				_ = writeMsg(c, protocol.BackendCopyData, payload)
			}
			_ = writeMsg(c, protocol.BackendCopyDone, nil)
			_ = writeCommandComplete(c, fmt.Sprintf("COPY %d", wantRows))
			_ = writeReadyForQuery(c, 'I')
			_, _ = io.Copy(io.Discard, c)
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	var got [][]byte
	err = conn.copyTo(ctx, "COPY t TO STDOUT", func(row []byte) error {
		got = append(got, row)
		return nil
	})
	if err != nil {
		t.Fatalf("copyTo: %v", err)
	}
	if len(got) != wantRows {
		t.Fatalf("got %d rows, want %d", len(got), wantRows)
	}
	if string(got[0]) != "row0\n" {
		t.Errorf("first row = %q, want %q", got[0], "row0\n")
	}
}

func TestCopyToAbortMidStream(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			typ, _, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			_ = writeMsg(c, protocol.BackendCopyOutResponse, []byte{0, 0, 1, 0, 0})
			for i := 0; i < 10; i++ {
				_ = writeMsg(c, protocol.BackendCopyData, []byte(fmt.Sprintf("row%d\n", i)))
			}
			_ = writeMsg(c, protocol.BackendCopyDone, nil)
			_ = writeCommandComplete(c, "COPY 10")
			_ = writeReadyForQuery(c, 'I')
			_, _ = io.Copy(io.Discard, c)
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	stop := errors.New("stop")
	err = conn.copyTo(ctx, "COPY t TO STDOUT", func(row []byte) error {
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("got %v, want stop", err)
	}
}

func TestSavepointNameValidation(t *testing.T) {
	bad := []string{
		"",
		"bad;name",
		"name with spaces",
		"drop",
		"dr'op",
	}
	// "drop" is actually valid by rule (only alnum+underscore), so replace it.
	bad = bad[:len(bad)-2]
	bad = append(bad, "bad name", "bad-dash", "bad.dot")
	for _, n := range bad {
		if err := validateSavepointName(n); err == nil {
			t.Errorf("validateSavepointName(%q) accepted invalid name", n)
		}
	}
	good := []string{"sp1", "MyPoint", "_under", "abc123"}
	for _, n := range good {
		if err := validateSavepointName(n); err != nil {
			t.Errorf("validateSavepointName(%q) rejected: %v", n, err)
		}
	}
}

func TestSavepointTxWireBytes(t *testing.T) {
	var queriesMu sync.Mutex
	var queries []string
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil {
					return
				}
				if typ != protocol.MsgQuery {
					if typ == protocol.MsgTerminate {
						return
					}
					continue
				}
				// body is cstring: SQL\0.
				s := string(body)
				s = strings.TrimRight(s, "\x00")
				queriesMu.Lock()
				queries = append(queries, s)
				queriesMu.Unlock()
				_ = writeCommandComplete(c, "SAVEPOINT")
				_ = writeReadyForQuery(c, 'T')
			}
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Savepoint(ctx, "sp1"); err != nil {
		t.Fatalf("Savepoint: %v", err)
	}
	if err := conn.RollbackToSavepoint(ctx, "sp1"); err != nil {
		t.Fatalf("RollbackToSavepoint: %v", err)
	}
	if err := conn.ReleaseSavepoint(ctx, "sp1"); err != nil {
		t.Fatalf("ReleaseSavepoint: %v", err)
	}
	// Invalid name must not reach the wire.
	if err := conn.Savepoint(ctx, "bad name"); err == nil {
		t.Fatalf("expected error for bad savepoint name")
	}

	want := []string{"SAVEPOINT sp1", "ROLLBACK TO SAVEPOINT sp1", "RELEASE SAVEPOINT sp1"}
	queriesMu.Lock()
	gotQueries := append([]string(nil), queries...)
	queriesMu.Unlock()
	if len(gotQueries) != len(want) {
		t.Fatalf("got %d queries, want %d: %v", len(gotQueries), len(want), gotQueries)
	}
	for i, q := range want {
		if gotQueries[i] != q {
			t.Errorf("query %d = %q, want %q", i, gotQueries[i], q)
		}
	}
}
