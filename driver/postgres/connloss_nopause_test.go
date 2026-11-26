package postgres

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

func noPauseIters() int {
	if v, err := strconv.Atoi(os.Getenv("CELERIS_NOPAUSE_ITERS")); err == nil && v > 0 {
		return v
	}
	return 40
}

// A cut reply is an error that matches ErrClosed and io.ErrUnexpectedEOF however
// the end of the connection arrives, also when the reader finds the data and the
// FIN or the reset together (no pause before the close). On the event loop's sync
// path the poll that read the reply returns the raw read error (ErrUnknownFD after
// the worker tore the fd down, ECONNRESET), and the caller used to return it
// when its failReq stored it before the failAll that the same poll triggered:
// a bare errno for a result that was cut off. Each cell repeats the cut, as the
// race is a matter of timing.
func TestConnLossNoPauseCutIsLossError(t *testing.T) {
	kinds := []lossKind{lossAfterLast, lossAfterC, lossMid, lossBeforeFirst, lossNothing, lossFatal, lossRandomCut}
	for _, carrier := range lossCarriers() {
		if carrier.name == "sqlConn" || carrier.name == "sqlTx" || carrier.name == "poolTx" {
			continue // the same query path; the cells below are the carriers that differ
		}
		for _, engine := range []string{"loop", "direct"} {
			if engine == "direct" && !carrier.directOK {
				continue
			}
			for _, ext := range []bool{false, true} {
				for _, kind := range kinds {
					for _, rst := range []bool{false, true} {
						cut := "FIN"
						if rst {
							cut = "RST"
						}
						carrier, engine, ext, kind, rst := carrier, engine, ext, kind, rst
						name := fmt.Sprintf("%s/%s/ext=%v/%s/%s", carrier.name, engine, ext, kind, cut)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							cfg := lossCfg{kind: kind, n: 50, rst: rst, noPause: true}
							if kind == lossRandomCut {
								cfg.n = 20000 // a reply of several reads
							}
							for i := 0; i < noPauseIters(); i++ {
								addr := startFakePG(t, lossServer(t, cfg))
								rows, err := carrier.run(t, addr, engine == "direct", ext, false)
								checkLoss(t, cfg, rows, err)
								if t.Failed() {
									t.Fatalf("iteration %d", i)
								}
							}
						})
					}
				}
			}
		}
	}
}

// TestFailedErrIsLossError is the deterministic form of the race above: the
// caller's failReq stored the raw poll error before the failAll that the same
// poll triggered (the loop worker had marked the conn closed first, so the poll's
// own errorClose did not fire onClose, and its read returned ErrUnknownFD), so
// failAll finds the request done and the first error stands. What the caller
// returns for it is the connection-loss error, never the bare errno.
func TestFailedErrIsLossError(t *testing.T) {
	for _, raw := range []error{engine.ErrUnknownFD, syscall.ECONNRESET, syscall.EPIPE, io.EOF} {
		req := acquirePgRequest()
		req.fail(raw) // failReq's fail, first
		req.fail(lostError(io.EOF))
		err := req.failedErr(raw)
		if !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("failedErr after a failed %v = %v: want ErrClosed and io.ErrUnexpectedEOF", raw, err)
		}
		if errors.Is(err, io.EOF) {
			t.Errorf("failedErr after a failed %v = %v: matches io.EOF", raw, err)
		}
		if raw != io.EOF && !errors.Is(err, raw) {
			t.Errorf("failedErr after a failed %v = %v: the cause is lost", raw, err)
		}
		releasePgRequest(req)
		// No error stored: the poll's own error is the answer.
		req = acquirePgRequest()
		if err := req.failedErr(raw); !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("failedErr(%v) without a stored error = %v", raw, err)
		}
		releasePgRequest(req)
	}
}
