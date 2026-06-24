package postgres

import (
	"context"
	"database/sql/driver"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// TestIsCacheableWrite pins the write-side cache predicate: only single-verb
// INSERT/UPDATE/DELETE (after leading whitespace/comments, with a word
// boundary) are cacheable; reads, DDL, SET, TRUNCATE, CTE-wrapped writes and
// identifier look-alikes are not.
func TestIsCacheableWrite(t *testing.T) {
	t.Parallel()
	cacheable := []string{
		"INSERT INTO t(a) VALUES($1)",
		"  update t set a=$1 where id=$2",
		"DELETE FROM t WHERE id=$1",
		"/* hint */ INSERT INTO t VALUES($1)",
		"-- comment\nINSERT INTO t VALUES($1)",
	}
	notCacheable := []string{
		"SELECT 1",
		"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", // starts WITH, not a plain write verb
		"CREATE TABLE t(a int)",
		"SET search_path = x",
		"TRUNCATE t",
		"UPDATELOG SET a=1", // word boundary: must NOT match UPDATE
		"INSERTED INTO t",   // word boundary: must NOT match INSERT
		"",
	}
	for _, q := range cacheable {
		if !isCacheableWrite(q) {
			t.Errorf("isCacheableWrite(%q) = false, want true", q)
		}
	}
	for _, q := range notCacheable {
		if isCacheableWrite(q) {
			t.Errorf("isCacheableWrite(%q) = true, want false", q)
		}
	}
}

// TestExecContextAutoCacheSkipsParse is the regression guard for the v1.5.5
// pg-write quick win: with AutoCacheStatements on, a repeated parameterized
// INSERT through conn.ExecContext must Parse exactly once and reuse the
// prepared statement (Bind+Execute) thereafter — symmetric with QueryContext.
// Before the fix, ExecContext always sent an unnamed statement, so every call
// re-parsed and the server re-planned, the gap behind pgx on driver-pg-write.
func TestExecContextAutoCacheSkipsParse(t *testing.T) {
	var parseCount atomic.Int32
	srv := func(c net.Conn) {
		fakePGTrustStartupB(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgParse:
					parseCount.Add(1)
					_ = writeMsg(c, protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					_ = writeMsg(c, protocol.BackendParameterDesc, buildParameterDescription([]uint32{protocol.OIDText}))
					_ = writeMsg(c, protocol.BackendNoData, nil)
				case protocol.MsgBind:
					_ = writeMsg(c, protocol.BackendBindComplete, nil)
				case protocol.MsgExecute:
					_ = writeCommandComplete(c, "INSERT 0 1")
				case protocol.MsgSync:
					_ = writeReadyForQuery(c, 'I')
				case protocol.MsgClose:
					_ = writeMsg(c, protocol.BackendCloseComplete, nil)
				}
			}
		})
	}
	addr := startFakePG(t, srv)
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{
		SSLMode:             "disable",
		StatementCacheSize:  16,
		AutoCacheStatements: true,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	const q = "INSERT INTO bench_writes(payload) VALUES($1)"
	args := []driver.NamedValue{{Ordinal: 1, Value: "x"}}
	for i := 0; i < 3; i++ {
		if _, err := c.ExecContext(ctx, q, args); err != nil {
			t.Fatalf("ExecContext #%d: %v", i, err)
		}
	}
	if n := parseCount.Load(); n != 1 {
		t.Errorf("Parse count = %d across 3 identical ExecContext calls, want 1 (statement must be cached after the first prepare)", n)
	}
}
