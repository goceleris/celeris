//go:build postgres

package postgres_test

// A backend that dies mid-result must not look like a short, complete result.
//
// pg_terminate_backend from a second connection (the backend sends a FATAL 57P01
// ErrorResponse and closes), and a TCP proxy that forwards every DataRow and cuts
// (FIN or RST) just before CommandComplete, while a result is in flight, on every
// carrier: database/sql DB, Conn and Tx, the native Pool and Tx on the event
// loop and in direct mode, QueryRow, simple and extended protocol, a result
// that buffers (under 64 rows) and one that streams. The assertion everywhere:
// rows read < rows the query produces, or the completion not seen, means a
// non-nil error. Rows carry a marker (g = 1..N) that is checked row by row.
//
// CELERIS_LOSS_ITERS (default 2) repeats each cell.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	postgres "github.com/goceleris/celeris/driver/postgres"
)

func lossIters() int {
	if s := os.Getenv("CELERIS_LOSS_ITERS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

var (
	lossSrvOnce sync.Once
	lossSrv     *celeris.Server
)

// lossLoopProvider is a server that is not started: a Pool opened with it runs on
// the driver's own event loop, the loop path.
func lossLoopProvider() postgres.ServerProvider {
	lossSrvOnce.Do(func() { lossSrv = celeris.New(celeris.Config{Addr: "127.0.0.1:0"}) })
	return lossSrv
}

// lossProxy forwards a client to the server and, when armed, cuts the connection
// right before the next CommandComplete from the server: every DataRow before it
// is forwarded, the completion never is.
type lossProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	arm   atomic.Int32 // 0 off, 1 FIN, 2 RST
	cuts  atomic.Int32
}

func startLossProxy(t *testing.T, target string) *lossProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &lossProxy{ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, s)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(s, c); _ = s.Close() }()
			go func() { p.serverToClient(c, s); _ = c.Close(); _ = s.Close() }()
		}
	}()
	return p
}

func (p *lossProxy) serverToClient(c, s net.Conn) {
	buf := make([]byte, 64<<10)
	var pend []byte
	for {
		n, err := s.Read(buf)
		if n > 0 {
			pend = append(pend, buf[:n]...)
			for len(pend) >= 5 {
				l := int(binary.BigEndian.Uint32(pend[1:5]))
				if pend[0] == 'C' {
					if a := p.arm.Load(); a != 0 {
						p.cuts.Add(1)
						if tc, ok := c.(*net.TCPConn); ok && a == 2 {
							_ = tc.SetLinger(0)
						}
						return
					}
				}
				if len(pend) < 1+l {
					break
				}
				if _, werr := c.Write(pend[:1+l]); werr != nil {
					return
				}
				pend = pend[1+l:]
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *lossProxy) addr() string { return p.ln.Addr().String() }

func lossDSNVia(t *testing.T, dsn, hostport string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Skipf("DSN is not a URL: %v", err)
	}
	u.Host = hostport
	return u.String()
}

func lossDSNHostPort(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Skipf("DSN is not a URL: %v", err)
	}
	h := u.Host
	if u.Port() == "" {
		h += ":5432"
	}
	return h
}

const lossPad = "repeat('x', 300)"

// lossQuery returns the statement of n rows (g, pad) and its arguments. A kill
// point "mid" sleeps ten seconds at row n/2, "first" sleeps before row 1, and
// "none" does not sleep (so the rows come at once: used with the proxy).
func lossQuery(n int, point string, ext bool) (string, []any) {
	var sleepAt string
	switch point {
	case "mid":
		sleepAt = fmt.Sprintf("g = %d", n/2)
	case "first":
		sleepAt = "g = 1"
	default:
		sleepAt = "false"
	}
	q := fmt.Sprintf("SELECT g::bigint, %s || CASE WHEN %s THEN (SELECT '' FROM pg_sleep(10)) ELSE '' END FROM generate_series(1, %d) g", lossPad, sleepAt, n)
	if ext {
		return q + " WHERE $1::int > 0", []any{1}
	}
	return q, nil
}

// lossSession is one connection of a carrier: its backend pid, and the statement run
// on it.
type lossSession struct {
	pid    func(ctx context.Context) (int, error)
	rows   func(ctx context.Context, q string, args []any) (n int, err error)
	rowOne func(ctx context.Context, q string, args []any) (v int64, err error)
}

type lossCarrier struct {
	name string
	open func(t *testing.T, dsn string) *lossSession
}

func lossSQLRows(rows *sql.Rows) (int, error) {
	n := 0
	for rows.Next() {
		var g int64
		var pad string
		if err := rows.Scan(&g, &pad); err != nil {
			_ = rows.Close()
			return n, err
		}
		if g != int64(n+1) {
			_ = rows.Close()
			return n, fmt.Errorf("row %d carries marker %d", n+1, g)
		}
		n++
	}
	err := rows.Err()
	_ = rows.Close()
	return n, err
}

func lossPoolRows(rows *postgres.Rows) (int, error) {
	n := 0
	for rows.Next() {
		var g int64
		var pad string
		if err := rows.Scan(&g, &pad); err != nil {
			_ = rows.Close()
			return n, err
		}
		if g != int64(n+1) {
			_ = rows.Close()
			return n, fmt.Errorf("row %d carries marker %d", n+1, g)
		}
		n++
	}
	err := rows.Err()
	_ = rows.Close()
	return n, err
}

func lossCarriers() []lossCarrier {
	sqlDB := func(t *testing.T, dsn string) *sql.DB {
		db, err := openSQLEngineForLoss(dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	pool := func(t *testing.T, dsn string, loop bool) *postgres.Pool {
		opts := []postgres.Option{postgres.WithMaxOpen(1)}
		if loop {
			opts = append(opts, postgres.WithEngine(lossLoopProvider()))
		}
		p, err := postgres.Open(dsn, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	var cs []lossCarrier
	cs = append(cs, lossCarrier{"sqlDB", func(t *testing.T, dsn string) *lossSession {
		db := sqlDB(t, dsn)
		return &lossSession{
			pid: func(ctx context.Context) (int, error) {
				var p int
				err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&p)
				return p, err
			},
			rows: func(ctx context.Context, q string, args []any) (int, error) {
				r, err := db.QueryContext(ctx, q, args...)
				if err != nil {
					return 0, err
				}
				return lossSQLRows(r)
			},
			rowOne: func(ctx context.Context, q string, args []any) (int64, error) {
				var g int64
				var pad string
				err := db.QueryRowContext(ctx, q, args...).Scan(&g, &pad)
				return g, err
			},
		}
	}})
	cs = append(cs, lossCarrier{"sqlConn", func(t *testing.T, dsn string) *lossSession {
		db := sqlDB(t, dsn)
		var conn *sql.Conn
		get := func(ctx context.Context) *sql.Conn {
			if conn == nil {
				c, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				conn = c
			}
			return conn
		}
		return &lossSession{
			pid: func(ctx context.Context) (int, error) {
				var p int
				err := get(ctx).QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&p)
				return p, err
			},
			rows: func(ctx context.Context, q string, args []any) (int, error) {
				r, err := get(ctx).QueryContext(ctx, q, args...)
				if err != nil {
					return 0, err
				}
				return lossSQLRows(r)
			},
			rowOne: func(ctx context.Context, q string, args []any) (int64, error) {
				var g int64
				var pad string
				err := get(ctx).QueryRowContext(ctx, q, args...).Scan(&g, &pad)
				return g, err
			},
		}
	}})
	cs = append(cs, lossCarrier{"sqlTx", func(t *testing.T, dsn string) *lossSession {
		db := sqlDB(t, dsn)
		var tx *sql.Tx
		get := func(ctx context.Context) *sql.Tx {
			if tx == nil {
				x, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				tx = x
			}
			return tx
		}
		return &lossSession{
			pid: func(ctx context.Context) (int, error) {
				var p int
				err := get(ctx).QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&p)
				return p, err
			},
			rows: func(ctx context.Context, q string, args []any) (int, error) {
				r, err := get(ctx).QueryContext(ctx, q, args...)
				if err != nil {
					return 0, err
				}
				return lossSQLRows(r)
			},
			rowOne: func(ctx context.Context, q string, args []any) (int64, error) {
				var g int64
				var pad string
				err := get(ctx).QueryRowContext(ctx, q, args...).Scan(&g, &pad)
				return g, err
			},
		}
	}})
	for _, mode := range []struct {
		name string
		loop bool
	}{{"loop", true}, {"direct", false}} {
		loop := mode.loop
		cs = append(cs, lossCarrier{"poolQuery-" + mode.name, func(t *testing.T, dsn string) *lossSession {
			p := pool(t, dsn, loop)
			return &lossSession{
				pid: func(ctx context.Context) (int, error) {
					var pid int
					err := p.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
					return pid, err
				},
				rows: func(ctx context.Context, q string, args []any) (int, error) {
					r, err := p.QueryContext(ctx, q, args...)
					if err != nil {
						return 0, err
					}
					return lossPoolRows(r)
				},
				rowOne: func(ctx context.Context, q string, args []any) (int64, error) {
					var g int64
					var pad string
					err := p.QueryRow(ctx, q, args...).Scan(&g, &pad)
					return g, err
				},
			}
		}})
		cs = append(cs, lossCarrier{"poolTx-" + mode.name, func(t *testing.T, dsn string) *lossSession {
			p := pool(t, dsn, loop)
			var tx *postgres.Tx
			get := func(ctx context.Context) *postgres.Tx {
				if tx == nil {
					x, err := p.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					tx = x
				}
				return tx
			}
			return &lossSession{
				pid: func(ctx context.Context) (int, error) {
					var pid int
					err := get(ctx).QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
					return pid, err
				},
				rows: func(ctx context.Context, q string, args []any) (int, error) {
					r, err := get(ctx).QueryContext(ctx, q, args...)
					if err != nil {
						return 0, err
					}
					return lossPoolRows(r)
				},
				rowOne: func(ctx context.Context, q string, args []any) (int64, error) {
					var g int64
					var pad string
					err := get(ctx).QueryRow(ctx, q, args...).Scan(&g, &pad)
					return g, err
				},
			}
		}})
	}
	return cs
}

func openSQLEngineForLoss(dsn string) (*sql.DB, error) {
	c, err := postgres.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	// database/sql is the loop path: the connector without an engine uses the
	// driver's own event loop.
	return sql.OpenDB(c), nil
}

func lossCheck(t *testing.T, got, want int, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: Err()==nil after %d of %d rows: a cut-off result ended as if complete", what, got, want)
		return
	}
	if err == io.EOF {
		t.Errorf("%s: the error is a bare io.EOF", what)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Errorf("%s: the error is sql.ErrNoRows: %v", what, err)
	}
}

// terminate kills the backend of pid from another connection, after delay.
func lossTerminateAfter(t *testing.T, admin *sql.DB, pid int, delay time.Duration) {
	t.Helper()
	time.AfterFunc(delay, func() {
		var ok bool
		_ = admin.QueryRow("SELECT pg_terminate_backend($1)", pid).Scan(&ok)
	})
}

func TestConnLossTerminateBackend(t *testing.T) {
	dsn := dsnFromEnv(t)
	admin := openDB(t)
	for _, carrier := range lossCarriers() {
		for _, ext := range []bool{false, true} {
			for _, shape := range []struct {
				name string
				n    int
			}{{"buffered", 50}, {"streaming", 3000}} {
				for _, point := range []string{"mid", "first"} {
					for it := 0; it < lossIters(); it++ {
						name := fmt.Sprintf("%s/ext=%v/%s/%s/%d", carrier.name, ext, shape.name, point, it)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
							defer cancel()
							s := carrier.open(t, dsn)
							pid, err := s.pid(ctx)
							if err != nil {
								t.Fatalf("pid: %v", err)
							}
							q, args := lossQuery(shape.n, point, ext)
							lossTerminateAfter(t, admin, pid, 300*time.Millisecond)
							rows, err := s.rows(ctx, q, args)
							lossCheck(t, rows, shape.n, err, "rows")
							if rows >= shape.n {
								t.Errorf("read %d of %d rows", rows, shape.n)
							}
							var pe *postgres.PGError
							if err != nil && errors.As(err, &pe) && pe.Code != "57P01" {
								t.Errorf("server error %v, want 57P01", pe)
							}
						})
						t.Run(name+"/queryRow", func(t *testing.T) {
							t.Parallel()
							ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
							defer cancel()
							s := carrier.open(t, dsn)
							pid, err := s.pid(ctx)
							if err != nil {
								t.Fatalf("pid: %v", err)
							}
							q, args := lossQuery(shape.n, point, ext)
							lossTerminateAfter(t, admin, pid, 300*time.Millisecond)
							_, err = s.rowOne(ctx, q, args)
							lossCheck(t, 0, shape.n, err, "QueryRow")
						})
					}
				}
			}
		}
	}
}

// TestConnLossCutBeforeCommandComplete: a proxy forwards every DataRow and cuts,
// FIN or RST, right before CommandComplete. All rows arrived, the completion did
// not: the statement may have failed at its end, so it is an error.
func TestConnLossCutBeforeCommandComplete(t *testing.T) {
	dsn := dsnFromEnv(t)
	_ = openDB(t)
	for _, carrier := range lossCarriers() {
		for _, ext := range []bool{false, true} {
			for _, n := range []int{50, 3000} {
				for _, rst := range []bool{false, true} {
					for it := 0; it < lossIters(); it++ {
						cut := "FIN"
						if rst {
							cut = "RST"
						}
						name := fmt.Sprintf("%s/ext=%v/rows%d/%s/%d", carrier.name, ext, n, cut, it)
						for _, one := range []bool{false, true} {
							nm := name
							if one {
								nm += "/queryRow"
							}
							t.Run(nm, func(t *testing.T) {
								t.Parallel()
								ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
								defer cancel()
								px := startLossProxy(t, lossDSNHostPort(t, dsn))
								s := carrier.open(t, lossDSNVia(t, dsn, px.addr()))
								// The conn is open and its session statements are done before the cut is armed.
								if _, err := s.pid(ctx); err != nil {
									t.Fatalf("pid: %v", err)
								}
								q, args := lossQuery(n, "none", ext)
								if rst {
									px.arm.Store(2)
								} else {
									px.arm.Store(1)
								}
								if one {
									_, err := s.rowOne(ctx, q, args)
									lossCheck(t, 0, n, err, "QueryRow")
								} else {
									rows, err := s.rows(ctx, q, args)
									lossCheck(t, rows, n, err, "rows")
									if err != nil && rows > n {
										t.Errorf("read %d of %d rows", rows, n)
									}
								}
								if px.cuts.Load() == 0 {
									t.Errorf("the proxy never cut: the test did not exercise the cut")
								}
							})
						}
					}
				}
			}
		}
	}
}

// TestConnLossNoFalseError: a connection that closes after a result completed does
// not turn that result into an error.
func TestConnLossNoFalseError(t *testing.T) {
	dsn := dsnFromEnv(t)
	for _, carrier := range lossCarriers() {
		for _, ext := range []bool{false, true} {
			for _, n := range []int{50, 3000} {
				t.Run(fmt.Sprintf("%s/ext=%v/rows%d", carrier.name, ext, n), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					s := carrier.open(t, dsn)
					q, args := lossQuery(n, "none", ext)
					rows, err := s.rows(ctx, q, args)
					if err != nil || rows != n {
						t.Fatalf("a complete result: %d of %d rows, err %v", rows, n, err)
					}
					v, err := s.rowOne(ctx, q, args)
					if err != nil || v != 1 {
						t.Fatalf("a complete QueryRow: %d, %v", v, err)
					}
				})
			}
		}
	}
}
