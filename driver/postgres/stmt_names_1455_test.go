package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// Named statements are named by the conn that prepares them. Behind a pooler
// that shares server connections between clients and does not track prepared
// statements (PgBouncer in transaction mode with max_prepared_statements=0),
// two clients that both named a statement celst_1 ran each other's SQL
// (celeris#1455). These tests pin the name: a per-connection random prefix
// drawn once at connection setup, then a counter.

var stmtNameRE = regexp.MustCompile(`^celst_[A-Z2-7]{26}_[0-9]+$`)

// cstr returns the NUL-terminated string at the start of b and the rest.
func cstr(b []byte) (string, []byte) {
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	if i == len(b) {
		return string(b), nil
	}
	return string(b[:i]), b[i+1:]
}

// nameRecorder is a fake backend that records every statement and portal name
// the driver puts on the wire, per server connection.
type nameRecorder struct {
	mu      sync.Mutex
	parse   [][]string // per accepted connection: statement names of Parse, in order
	bind    [][]string // statement names of Bind
	portals []string   // portal names of Bind
	closeS  [][]string // names of Close 'S'
	other   []string   // Describe/Execute names that must be unnamed
}

func (r *nameRecorder) handler(t *testing.T) func(net.Conn) {
	return func(c net.Conn) {
		r.mu.Lock()
		idx := len(r.parse)
		r.parse = append(r.parse, nil)
		r.bind = append(r.bind, nil)
		r.closeS = append(r.closeS, nil)
		r.mu.Unlock()
		fakePGTrustStartup(t, c, int32(idx+1), 2, func(c net.Conn) {
			var buf []byte
			enq := func(typ byte, payload []byte) {
				msg := make([]byte, 5+len(payload))
				msg[0] = typ
				msg[1], msg[2], msg[3], msg[4] = byte((4+len(payload))>>24), byte((4+len(payload))>>16), byte((4+len(payload))>>8), byte(4+len(payload))
				copy(msg[5:], payload)
				buf = append(buf, msg...)
			}
			for {
				typ, payload, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgParse:
					name, _ := cstr(payload)
					r.mu.Lock()
					r.parse[idx] = append(r.parse[idx], name)
					r.mu.Unlock()
					enq(protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					kind := payload[0]
					name, _ := cstr(payload[1:])
					if kind == 'P' && name != "" {
						r.mu.Lock()
						r.other = append(r.other, "describe portal "+name)
						r.mu.Unlock()
					}
					enq(protocol.BackendParameterDesc, buildParameterDescription([]uint32{protocol.OIDInt8}))
					enq(protocol.BackendNoData, nil)
				case protocol.MsgBind:
					portal, rest := cstr(payload)
					stmt, _ := cstr(rest)
					r.mu.Lock()
					r.portals = append(r.portals, portal)
					r.bind[idx] = append(r.bind[idx], stmt)
					r.mu.Unlock()
					enq(protocol.BackendBindComplete, nil)
				case protocol.MsgExecute:
					portal, _ := cstr(payload)
					if portal != "" {
						r.mu.Lock()
						r.other = append(r.other, "execute portal "+portal)
						r.mu.Unlock()
					}
					msg := append([]byte("INSERT 0 1"), 0)
					enq(protocol.BackendCommandComplete, msg)
				case protocol.MsgClose:
					name, _ := cstr(payload[1:])
					if payload[0] == 'S' {
						r.mu.Lock()
						r.closeS[idx] = append(r.closeS[idx], name)
						r.mu.Unlock()
					}
					enq(protocol.BackendCloseComplete, nil)
				case protocol.MsgSync:
					enq(protocol.BackendReadyForQuery, []byte{'I'})
					_, _ = c.Write(buf)
					buf = buf[:0]
				case protocol.MsgQuery:
					_ = writeCommandComplete(c, "SELECT 0")
					_ = writeReadyForQuery(c, 'I')
				}
			}
		})
	}
}

func TestStmtNamesAreConnectionUnique1455(t *testing.T) {
	var rec nameRecorder
	addr := startFakePG(t, rec.handler(t))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 2, AutoCacheStatements: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const conns = 6
	prefixes := map[string]int{}
	evictedWant := make([]string, 0, conns)
	for i := 0; i < conns; i++ {
		c, err := dialConn(ctx, prov, nil, dsn, 0)
		if err != nil {
			t.Fatal(err)
		}
		args := []driver.NamedValue{{Ordinal: 1, Value: int64(1)}}
		// The three minting sites, in order: an auto-cached Exec, an
		// auto-cached Query, an explicit Prepare. The cache holds 2, so the
		// third insert evicts the first (a Close 'S' with the full name).
		if _, err := c.ExecContext(ctx, fmt.Sprintf("INSERT INTO t(a) VALUES($1) /* %d */", i), args); err != nil {
			t.Fatalf("ExecContext: %v", err)
		}
		first, ok := c.stmtCache.get(fmt.Sprintf("INSERT INTO t(a) VALUES($1) /* %d */", i))
		if !ok {
			t.Fatal("Exec did not cache")
		}
		evictedWant = append(evictedWant, first.Name)
		rows, err := c.QueryContext(ctx, fmt.Sprintf("SELECT a FROM t WHERE a = $1 /* %d */", i), args)
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		_ = rows.Close()
		st, err := c.PrepareContext(ctx, fmt.Sprintf("SELECT a FROM t WHERE a > $1 /* %d */", i))
		if err != nil {
			t.Fatalf("PrepareContext: %v", err)
		}
		// Run the cached statements again: cache hits mint nothing.
		if _, err := st.(*pgStmt).ExecContext(ctx, args); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecContext(ctx, fmt.Sprintf("INSERT INTO t(a) VALUES($1) /* %d */", i), args); err != nil {
			t.Fatal(err)
		}
		prefixes[c.stmtPrefix]++
		if got := c.stmtCounter; got != 4 {
			// Exec miss, Query miss, Prepare, then the Exec of the evicted text
			// misses again: four mints, none on a hit.
			t.Errorf("conn %d: stmtCounter = %d, want 4", i, got)
		}
		_ = c.Close()
	}
	if len(prefixes) != conns {
		t.Errorf("%d distinct prefixes over %d conns", len(prefixes), conns)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.other) != 0 {
		t.Errorf("named portals on the wire: %v", rec.other)
	}
	for _, p := range rec.portals {
		if p != "" {
			t.Errorf("Bind names portal %q, want the unnamed portal", p)
		}
	}
	all := map[string]int{}
	for i := range rec.parse {
		var prefix string
		var last uint64
		for _, n := range rec.parse[i] {
			if n == "" {
				continue
			}
			if !stmtNameRE.MatchString(n) {
				t.Errorf("conn %d: Parse name %q does not match %v", i, n, stmtNameRE)
			}
			if len(n) > 63 {
				t.Errorf("name %q is %d bytes, PostgreSQL keeps 63", n, len(n))
			}
			all[n]++
			cut := strings.LastIndexByte(n, '_')
			p, num := n[:cut], n[cut+1:]
			if prefix == "" {
				prefix = p
			} else if p != prefix {
				t.Errorf("conn %d changed its prefix: %q then %q", i, prefix, p)
			}
			v, _ := strconv.ParseUint(num, 10, 64)
			if v <= last {
				t.Errorf("conn %d: counter %d after %d", i, v, last)
			}
			last = v
		}
		// Every named Bind and every Close names a statement this conn parsed.
		parsed := map[string]bool{}
		for _, n := range rec.parse[i] {
			parsed[n] = true
		}
		for _, n := range rec.bind[i] {
			if n != "" && !parsed[n] {
				t.Errorf("conn %d: Bind of %q, never parsed by this conn", i, n)
			}
		}
		for _, n := range rec.closeS[i] {
			if !parsed[n] {
				t.Errorf("conn %d: Close of %q, never parsed by this conn", i, n)
			}
		}
		if len(rec.closeS[i]) == 0 {
			t.Errorf("conn %d: no eviction Close reached the server", i)
		}
	}
	for n, k := range all {
		if k != 1 {
			t.Errorf("name %q Parsed %d times", n, k)
		}
	}
	// The eviction Close carries the full minted name.
	for i, want := range evictedWant {
		found := false
		for _, n := range rec.closeS[i] {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("conn %d: no Close for the evicted %q; got %v", i, want, rec.closeS[i])
		}
	}
}

// TestStmtNamePrefixUnique: the prefix is crypto/rand, 128 bits, and does not
// repeat over many connections drawn in parallel (the property a pooler needs:
// two processes, or one process after a restart, never share a name).
func TestStmtNamePrefixUnique(t *testing.T) {
	const workers, each = 8, 25000
	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, each)
			for i := range local {
				local[i] = newStmtNamePrefix()
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range local {
				if _, dup := seen[p]; dup {
					t.Errorf("prefix %q drawn twice", p)
					return
				}
				seen[p] = struct{}{}
			}
		}()
	}
	wg.Wait()
	for p := range seen {
		if len(p) != len("celst_")+26 || !strings.HasPrefix(p, "celst_") {
			t.Fatalf("prefix %q: want celst_ and 26 base32 characters (128 bits)", p)
		}
		break
	}
}

// TestStmtNameLength: the longest name this driver can mint fits PostgreSQL's
// 63-byte identifier limit, which truncates a longer name silently (two long
// names that share their first 63 bytes would then be one statement).
func TestStmtNameLength(t *testing.T) {
	c := &pgConn{stmtPrefix: newStmtNamePrefix()}
	c.stmtCounter = math.MaxUint64 - 1
	n := c.mintStmtName()
	if len(n) > 63 {
		t.Fatalf("%q is %d bytes", n, len(n))
	}
	if !stmtNameRE.MatchString(n) {
		t.Fatalf("%q does not match %v", n, stmtNameRE)
	}
}

// TestStmtNameSurvivesReset: ResetSession (DISCARD ALL and a cache reset) does
// not rewind the counter, so a conn never gives one name to two statements.
func TestStmtNameSurvivesReset(t *testing.T) {
	var rec nameRecorder
	addr := startFakePG(t, rec.handler(t))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4, AutoCacheStatements: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	a := c.mintStmtName()
	prefix := c.stmtPrefix
	c.sessionDirty.Store(true)
	if err := c.ResetSession(ctx); err != nil {
		t.Fatal(err)
	}
	b := c.mintStmtName()
	if c.stmtPrefix != prefix || a == b || !strings.HasSuffix(a, "_1") || !strings.HasSuffix(b, "_2") {
		t.Fatalf("names %q then %q after a reset (prefix %q)", a, b, prefix)
	}
}

// TestStmtPrefixSetByEveryDial: all three constructors draw the prefix, and no
// two conns share it, so a conn built by any of them mints names no other conn
// mints.
func TestStmtPrefixSetByEveryDial(t *testing.T) {
	var rec nameRecorder
	addr := startFakePG(t, rec.handler(t))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4, AutoCacheStatements: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := map[string]string{}
	for name, dial := range map[string]func() (*pgConn, error){
		"dialConn":        func() (*pgConn, error) { return dialConn(ctx, prov, nil, dsn, 0) },
		"dialConnNetConn": func() (*pgConn, error) { return dialConnNetConn(ctx, netConnProvider(t), nil, dsn, 0) },
		"dialDirectConn":  func() (*pgConn, error) { return dialDirectConn(ctx, dsn) },
	} {
		for i := 0; i < 2; i++ {
			c, err := dial()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			n := c.mintStmtName()
			_ = c.Close()
			if !stmtNameRE.MatchString(n) || !strings.HasSuffix(n, "_1") {
				t.Errorf("%s: first name %q", name, n)
			}
			if prev, dup := seen[n]; dup {
				t.Errorf("%s and %s minted %q", name, prev, n)
			}
			seen[n] = name
		}
	}
}
