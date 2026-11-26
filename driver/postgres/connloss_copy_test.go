package postgres

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// COPY TO and COPY FROM end with an error when the connection is lost before
// the completion, as a query does: a dest that received half the rows and a nil
// error is the same bug. (Both were already errors; these tests keep them so.)

// copyKind is where the fake backend ends the COPY.
type copyKind string

const (
	copyComplete   copyKind = "complete"      // CommandComplete + ReadyForQuery, then a close: clean
	copyMid        copyKind = "midStream"     // half the rows, then close
	copyAfterLast  copyKind = "afterLastRow"  // every row, close before CopyDone
	copyAfterDone  copyKind = "afterCopyDone" // CopyDone, close before CommandComplete
	copyAfterC     copyKind = "afterC"        // CommandComplete, close before ReadyForQuery
	copyNoResponse copyKind = "noResponse"    // close before the CopyOutResponse / CopyInResponse
	copyFatal      copyKind = "fatal57P01"    // half the rows, FATAL ErrorResponse, close
)

var allCopyKinds = []copyKind{copyComplete, copyMid, copyAfterLast, copyAfterDone, copyAfterC, copyNoResponse, copyFatal}

func copyServer(tb testing.TB, kind copyKind, n int, rst bool) func(net.Conn) {
	cut := func(c net.Conn) {
		// A complete COPY is followed by the close at once; the others get a pause so
		// that the bytes written before the close are read before it.
		if kind != copyComplete {
			time.Sleep(20 * time.Millisecond)
		}
		lossCfg{rst: rst}.cut(c)
	}
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
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
					var out []byte
					add := func(t byte, p []byte) { out = append(out, pgMsg(t, p)...) }
					switch {
					case strings.Contains(q, "TO STDOUT"):
						if kind == copyNoResponse {
							cut(c)
							return
						}
						add(protocol.BackendCopyOutResponse, []byte{0, 0, 1, 0, 0})
						rows := n
						if kind == copyMid || kind == copyFatal {
							rows = n / 2
						}
						for i := 0; i < rows; i++ {
							add(protocol.BackendCopyData, []byte(strconv.Itoa(i+1)+"\n"))
						}
						switch kind {
						case copyComplete:
							add(protocol.BackendCopyDone, nil)
							add(protocol.BackendCommandComplete, append([]byte("COPY "+strconv.Itoa(n)), 0))
							add(protocol.BackendReadyForQuery, []byte{'I'})
						case copyAfterDone:
							add(protocol.BackendCopyDone, nil)
						case copyAfterC:
							add(protocol.BackendCopyDone, nil)
							add(protocol.BackendCommandComplete, append([]byte("COPY "+strconv.Itoa(n)), 0))
						case copyFatal:
							add(protocol.BackendErrorResponse, []byte("SFATAL\x00VFATAL\x00C57P01\x00Mterminating connection due to administrator command\x00\x00"))
						}
						_, _ = c.Write(out)
						cut(c)
						return
					case strings.Contains(q, "FROM STDIN"):
						if kind == copyNoResponse {
							cut(c)
							return
						}
						add(protocol.BackendCopyInResponse, []byte{0, 0, 1, 0, 0})
						_, _ = c.Write(out)
						out = out[:0]
						got := 0
						for {
							t, _, err := readMsg(c)
							if err != nil {
								return
							}
							if t == 'd' {
								got++
								if (kind == copyMid || kind == copyFatal) && got == n/2 {
									if kind == copyFatal {
										add(protocol.BackendErrorResponse, []byte("SFATAL\x00VFATAL\x00C57P01\x00Mterminating connection due to administrator command\x00\x00"))
										_, _ = c.Write(out)
									}
									cut(c)
									return
								}
								continue
							}
							if t == 'c' || t == 'f' {
								break
							}
						}
						switch kind {
						case copyComplete:
							add(protocol.BackendCommandComplete, append([]byte("COPY "+strconv.Itoa(n)), 0))
							add(protocol.BackendReadyForQuery, []byte{'I'})
						case copyAfterC:
							add(protocol.BackendCommandComplete, append([]byte("COPY "+strconv.Itoa(n)), 0))
						}
						_, _ = c.Write(out)
						cut(c)
						return
					default:
						add(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))
						add(protocol.BackendReadyForQuery, []byte{'I'})
						_, _ = c.Write(out)
					}
				}
			}
		})
	}
}

func TestConnLossCopyIsNotSuccess(t *testing.T) {
	for _, engine := range []string{"loop", "direct"} {
		for _, dir := range []string{"to", "from"} {
			for _, n := range []int{50, 500} {
				for _, kind := range allCopyKinds {
					for _, rst := range []bool{false, true} {
						cut := "FIN"
						if rst {
							cut = "RST"
						}
						name := fmt.Sprintf("%s/copy-%s/rows%d/%s/%s", engine, dir, n, kind, cut)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							addr := startFakePG(t, copyServer(t, kind, n, rst))
							p := lossPool(t, addr, engine == "direct")
							ctx := tctx(t)
							var err error
							got := 0
							if dir == "to" {
								err = p.CopyTo(ctx, "COPY (SELECT g FROM t) TO STDOUT", func(row []byte) error {
									got++
									return nil
								})
							} else {
								rows := make([][]any, n)
								for i := range rows {
									rows[i] = []any{int64(i + 1)}
								}
								var cnt int64
								cnt, err = p.CopyFrom(ctx, "t", []string{"g"}, CopyFromSlice(rows))
								got = int(cnt)
							}
							if kind == copyComplete {
								if err != nil || got != n {
									t.Errorf("a complete COPY: %d of %d rows, err %v", got, n, err)
								}
								return
							}
							if err == nil {
								t.Fatalf("COPY %s ended with a nil error after %d of %d rows (%s)", dir, got, n, kind)
							}
							if kind == copyFatal {
								return
							}
							checkLoss(t, lossCfg{kind: lossMid, n: n}, got, err)
						})
					}
				}
			}
		}
	}
}
