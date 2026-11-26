package postgres

// A result whose ReadyForQuery was read is complete however the connection ends
// right after it. On the event loop's sync path the poll that read the reply goes
// on to read the end of the connection (a FIN, or a reset) and returns that
// error; the call used to return it for a statement that had completed, a bare
// "connection reset by peer" for an INSERT that was committed. These tests cut the
// fake backend's reply right at and after ReadyForQuery, with no pause, by FIN and
// RST, with a reply delay that lands it in each WriteAndPoll phase (found by the
// reviewer of the PR; the cases are his).

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

type arEnd string

const (
	arRFQ      arEnd = "fullThenClose" // the whole reply then a close, no pause
	arRFQPart  arEnd = "rfqMinus1"     // rows, CC, the Z message minus its status byte
	arRFQHdr   arEnd = "rfqHeaderOnly" // rows, CC, the 'Z' byte only
	arCCPart   arEnd = "ccPartial"     // rows, half of CommandComplete
	arFullKeep arEnd = "fullKeepOpen"  // control: the whole reply, connection stays open
	arRFQSep   arEnd = "fullSepClose"  // whole reply, a 2 ms pause, then the close
)

type arCfg struct {
	end  arEnd
	n    int
	rst  bool
	exec bool
	// commit makes the COMMIT of a transaction the statement under test: its reply
	// (CommandComplete, ReadyForQuery) is followed at once by the close.
	commit bool
	delay  time.Duration // before the reply is written
}

func arServer(tb testing.TB, cfg arCfg, served *atomic.Int32) func(net.Conn) {
	cols := buildRowDescription([]colSpec{{name: "g", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText}})
	colsBin := buildRowDescription([]colSpec{{name: "g", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatBinary}})
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			status := byte('I')
			var buf []byte
			add := func(typ byte, payload []byte) { buf = append(buf, pgMsg(typ, payload)...) }
			cut := func() {
				if tc, ok := c.(*net.TCPConn); ok && cfg.rst {
					_ = tc.SetLinger(0)
				}
				_ = c.Close()
			}
			script := func(extended bool) bool {
				served.Add(1)
				n := cfg.n
				if cfg.exec {
					n = 0
				}
				if !cfg.exec && !extended {
					add(protocol.BackendRowDescription, cols)
				}
				for i := 0; i < n; i++ {
					v := []byte(strconv.Itoa(i + 1))
					if extended {
						v = binary.BigEndian.AppendUint64(nil, uint64(i+1))
					}
					add(protocol.BackendDataRow, buildDataRow([][]byte{v}))
				}
				tag := "SELECT " + strconv.Itoa(cfg.n)
				if cfg.exec {
					tag = "UPDATE " + strconv.Itoa(cfg.n)
				}
				cc := pgMsg(protocol.BackendCommandComplete, append([]byte(tag), 0))
				z := pgMsg(protocol.BackendReadyForQuery, []byte{status})
				switch cfg.end {
				case arRFQ, arFullKeep, arRFQSep:
					buf = append(buf, cc...)
					buf = append(buf, z...)
				case arRFQPart:
					buf = append(buf, cc...)
					buf = append(buf, z[:len(z)-1]...)
				case arRFQHdr:
					buf = append(buf, cc...)
					buf = append(buf, z[0])
				case arCCPart:
					buf = append(buf, cc[:len(cc)/2]...)
				}
				if cfg.delay > 0 {
					time.Sleep(cfg.delay)
				}
				_, _ = c.Write(buf)
				buf = buf[:0]
				switch cfg.end {
				case arFullKeep:
					return false
				case arRFQSep:
					time.Sleep(2 * time.Millisecond)
				}
				cut()
				return true
			}
			executed := false
			for {
				typ, body, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgQuery:
					q := string(body[:len(body)-1])
					if strings.HasPrefix(q, "SELECT g") || strings.HasPrefix(q, "UPDATE") {
						if script(false) {
							return
						}
						continue
					}
					switch q {
					case "BEGIN":
						status = 'T'
						add(protocol.BackendCommandComplete, append([]byte("BEGIN"), 0))
					case "COMMIT", "ROLLBACK":
						status = 'I'
						add(protocol.BackendCommandComplete, append([]byte(q), 0))
						if cfg.commit && q == "COMMIT" {
							add(protocol.BackendReadyForQuery, []byte{status})
							_, _ = c.Write(buf)
							buf = buf[:0]
							cut()
							return
						}
					default:
						add(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))
					}
					add(protocol.BackendReadyForQuery, []byte{status})
					_, _ = c.Write(buf)
					buf = buf[:0]
				case protocol.MsgParse:
					add(protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					if len(body) > 0 && body[0] == 'S' {
						add(protocol.BackendParameterDesc, buildParameterDescription([]uint32{protocol.OIDInt8}))
					}
					if cfg.exec {
						add(protocol.BackendNoData, nil)
					} else {
						add(protocol.BackendRowDescription, colsBin)
					}
				case protocol.MsgBind:
					add(protocol.BackendBindComplete, nil)
				case protocol.MsgExecute:
					executed = true
				case protocol.MsgSync:
					if !executed {
						add(protocol.BackendReadyForQuery, []byte{status})
						_, _ = c.Write(buf)
						buf = buf[:0]
						break
					}
					executed = false
					if script(true) {
						return
					}
				case protocol.MsgClose:
					add(protocol.BackendCloseComplete, nil)
					_, _ = c.Write(buf)
					buf = buf[:0]
				}
			}
		})
	}
}

type arCarrier struct {
	name   string
	direct bool
	sql    bool
}

func arCarriers() []arCarrier {
	return []arCarrier{{"sqlDB", false, true}, {"poolLoop", false, false}, {"poolDirect", true, false}}
}

// arRun runs one call of shape on the carrier; returns rows read, error.
func arRun(t *testing.T, db *sql.DB, p *Pool, shape string, ext bool, n int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q, args := lossQuery(shape == "exec", ext)
	switch shape {
	case "query":
		if db != nil {
			rows, err := db.QueryContext(ctx, q, args...)
			if err != nil {
				return 0, err
			}
			return readSQLRows(rows)
		}
		rows, err := p.QueryContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		return readPoolRows(rows)
	case "queryRow":
		var g int64
		var err error
		if db != nil {
			err = db.QueryRowContext(ctx, q, args...).Scan(&g)
		} else {
			err = p.QueryRow(ctx, q, args...).Scan(&g)
		}
		if err == nil && g != 1 {
			return 0, fmt.Errorf("marker %d", g)
		}
		if err == nil {
			return 1, nil
		}
		return 0, err
	case "exec":
		var err error
		if db != nil {
			_, err = db.ExecContext(ctx, q, args...)
		} else {
			_, err = p.ExecContext(ctx, q, args...)
		}
		return 0, err
	case "txCommit":
		if db != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return 0, fmt.Errorf("begin: %w", err)
			}
			return 0, tx.Commit()
		}
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin: %w", err)
		}
		return 0, tx.Commit()
	case "txQueryCommit":
		if db != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return 0, fmt.Errorf("begin: %w", err)
			}
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				_ = tx.Rollback()
				return 0, err
			}
			k, err := readSQLRows(rows)
			if err != nil {
				_ = tx.Rollback()
				return k, err
			}
			return k, tx.Commit()
		}
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin: %w", err)
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		k, err := readPoolRows(rows)
		if err != nil {
			_ = tx.Rollback()
			return k, err
		}
		return k, tx.Commit()
	}
	return 0, errors.New("bad shape")
}

func arIters() int {
	if s := os.Getenv("CELERIS_AFTERREADY_ITERS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return 20
}

// TestConnLossCloseAfterReadyIsComplete: a complete reply followed at once by a close
// reads as complete (no error), and the next call on the same pool does not hang.
func TestConnLossCloseAfterReadyIsComplete(t *testing.T) {
	delays := []time.Duration{0, 50 * time.Microsecond, 300 * time.Microsecond, 1500 * time.Microsecond, 5 * time.Millisecond}
	for _, car := range arCarriers() {
		for _, shape := range []string{"query", "queryRow", "exec", "txCommit"} {
			for _, n := range []int{50, 500} {
				if shape != "query" && n != 50 {
					continue
				}
				for _, ext := range []bool{false, true} {
					for _, rst := range []bool{false, true} {
						if rst && !rstAfterCompleteIsClean {
							continue
						}
						for _, end := range []arEnd{arRFQ, arRFQSep} {
							car, shape, n, ext, rst, end := car, shape, n, ext, rst, end
							name := fmt.Sprintf("%s/%s/n%d/ext=%v/rst=%v/%s", car.name, shape, n, ext, rst, end)
							t.Run(name, func(t *testing.T) {
								t.Parallel()
								rn := n
								if shape == "queryRow" {
									rn = 1
								}
								var falseErr, nextHang, nextErr, ok int
								var firstFalse, firstNext error
								for i := 0; i < arIters(); i++ {
									cfg := arCfg{end: end, n: rn, rst: rst, exec: shape == "exec", commit: shape == "txCommit", delay: delays[i%len(delays)]}
									var served atomic.Int32
									addr := startFakePG(t, arServer(t, cfg, &served))
									var db *sql.DB
									var p *Pool
									if car.sql {
										db = lossSQL(t, addr)
									} else {
										p = lossPool(t, addr, car.direct)
									}
									rows, err := arRun(t, db, p, shape, ext, rn)
									if err != nil {
										falseErr++
										if firstFalse == nil {
											firstFalse = fmt.Errorf("iter %d delay %v rows %d: %w", i, cfg.delay, rows, err)
										}
									} else if shape == "query" && rows != rn {
										t.Errorf("short result with nil error: %d of %d", rows, rn)
									} else {
										ok++
									}
									// The next call on the same pool (MaxOpen 1).
									_, err2 := arRun(t, db, p, shape, ext, rn)
									if err2 != nil {
										if errors.Is(err2, context.DeadlineExceeded) {
											nextHang++
										} else {
											nextErr++
											if firstNext == nil {
												firstNext = err2
											}
										}
									}
								}
								t.Logf("AFTERREADY %s ok=%d falseErr=%d nextErr=%d nextHang=%d firstFalse=%v firstNext=%v", name, ok, falseErr, nextErr, nextHang, firstFalse, firstNext)
								if falseErr > 0 {
									t.Errorf("FALSE ERROR for a complete result: %d of %d (first: %v)", falseErr, arIters(), firstFalse)
								}
								if nextHang > 0 {
									t.Errorf("next call hung %d times", nextHang)
								}
							})
						}
					}
				}
			}
		}
	}
}

// TestConnLossPartialReadyIsError: a reply cut inside CommandComplete or ReadyForQuery is
// an error matching ErrClosed and io.ErrUnexpectedEOF.
func TestConnLossPartialReadyIsError(t *testing.T) {
	for _, car := range arCarriers() {
		for _, shape := range []string{"query", "queryRow", "exec", "txQueryCommit"} {
			for _, ext := range []bool{false, true} {
				for _, rst := range []bool{false, true} {
					for _, end := range []arEnd{arRFQPart, arRFQHdr, arCCPart} {
						car, shape, ext, rst, end := car, shape, ext, rst, end
						name := fmt.Sprintf("%s/%s/ext=%v/rst=%v/%s", car.name, shape, ext, rst, end)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							rn := 50
							if shape == "queryRow" {
								rn = 1
							}
							for i := 0; i < 5; i++ {
								cfg := arCfg{end: end, n: rn, rst: rst, exec: shape == "exec", delay: time.Duration(i) * 400 * time.Microsecond}
								var served atomic.Int32
								addr := startFakePG(t, arServer(t, cfg, &served))
								var db *sql.DB
								var p *Pool
								if car.sql {
									db = lossSQL(t, addr)
								} else {
									p = lossPool(t, addr, car.direct)
								}
								rows, err := arRun(t, db, p, shape, ext, rn)
								if err == nil {
									t.Fatalf("iter %d: nil error after %d rows: a reply cut at %s read as complete", i, rows, end)
								}
								if errors.Is(err, context.DeadlineExceeded) {
									t.Fatalf("iter %d: hang: %v", i, err)
								}
								if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
									t.Errorf("iter %d: error %v (%T) does not match ErrClosed+ErrUnexpectedEOF", i, err, err)
								}
							}
						})
					}
				}
			}
		}
	}
}
