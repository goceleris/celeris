package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

func TestPool_OpenAndPing(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		for {
			typ, _, err := readMsg(c)
			if err != nil {
				return
			}
			if typ == protocol.MsgTerminate {
				return
			}
			if typ == protocol.MsgQuery {
				_ = writeCommandComplete(c, "SELECT 1")
				_ = writeReadyForQuery(c, 'I')
			}
		}
	})
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@" + host + ":" + port + "/d?sslmode=disable&statement_cache_size=4")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	s := p.Stats()
	if s.Open < 1 {
		t.Errorf("open = %d, want >= 1", s.Open)
	}
}

func TestPool_WithWorker(t *testing.T) {
	ctx := context.Background()
	ctx2 := WithWorker(ctx, 3)
	if got := workerFromCtx(ctx2); got != 3 {
		t.Errorf("workerFromCtx = %d, want 3", got)
	}
	if got := workerFromCtx(ctx); got != -1 {
		t.Errorf("workerFromCtx(bare) = %d, want -1", got)
	}
}

func TestPool_PingWithAffinity(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		for {
			typ, _, err := readMsg(c)
			if err == io.EOF {
				return
			}
			if err != nil {
				return
			}
			if typ == protocol.MsgTerminate {
				return
			}
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
		}
	})
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ctx := WithWorker(context.Background(), 0)
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestConvertAssignScanner(t *testing.T) {
	// sql.NullString with non-nil value.
	var ns sql.NullString
	if err := scanValue(&ns, "hello"); err != nil {
		t.Fatalf("scanValue NullString: %v", err)
	}
	if !ns.Valid || ns.String != "hello" {
		t.Fatalf("NullString = %+v, want {hello true}", ns)
	}

	// sql.NullString with nil value.
	ns = sql.NullString{String: "stale", Valid: true}
	if err := scanValue(&ns, nil); err != nil {
		t.Fatalf("scanValue NullString nil: %v", err)
	}
	if ns.Valid {
		t.Fatalf("NullString should be invalid after nil scan, got %+v", ns)
	}

	// sql.NullInt64 with non-nil value.
	var ni sql.NullInt64
	if err := scanValue(&ni, int64(42)); err != nil {
		t.Fatalf("scanValue NullInt64: %v", err)
	}
	if !ni.Valid || ni.Int64 != 42 {
		t.Fatalf("NullInt64 = %+v, want {42 true}", ni)
	}

	// Custom Scanner type.
	var cs customScanner
	if err := scanValue(&cs, "custom-val"); err != nil {
		t.Fatalf("scanValue customScanner: %v", err)
	}
	if cs.val != "custom-val" || !cs.called {
		t.Fatalf("customScanner = %+v", cs)
	}
}

type customScanner struct {
	val    string
	called bool
}

func (c *customScanner) Scan(src any) error {
	c.called = true
	if src == nil {
		c.val = ""
		return nil
	}
	c.val = fmt.Sprintf("%v", src)
	return nil
}
