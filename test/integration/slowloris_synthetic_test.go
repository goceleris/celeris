//go:build linux

package integration

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/resource"
)

// TestSlowlorisSynthetic is the focused per-engine reproducer for the
// v1.4.11 slowloris-defence work. Replaces the 1h+ probatorium nightly
// cycle with an in-process test that logs every relevant step.
//
// Scenario per engine:
//
//  1. Start celeris with engine=iouring/epoll/std, ReadHeaderTimeout=2s
//     (short so the test finishes fast).
//  2. Open a raw TCP client conn.
//  3. Send a partial HTTP/1.1 request preamble (no terminating \r\n\r\n).
//  4. Drip 'X' bytes every 200ms.
//  5. Record the FIRST write failure timestamp and which error it returned.
//  6. Compare actual close time to expected (~2s).
//
// Expected: each engine MUST close the conn at ~ReadHeaderTimeout, and
// the client MUST observe the close on its next write within reasonable
// slack (~500ms typical TCP-FIN-delivery latency).
//
// Failure modes the test logs explicitly:
//   - Close never observed within timeout (defence not firing).
//   - Close observed but much later than ReadHeaderTimeout (cadence issue).
//   - Close at deadline but client write succeeds for seconds after
//     (FIN-vs-RST issue — RST would force immediate fail).
func TestSlowlorisSynthetic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		newEng  func(cfg resource.Config) (engine.Engine, error)
		minSkip string // skip message if engine unsupported
	}{
		{
			name: "iouring",
			newEng: func(cfg resource.Config) (engine.Engine, error) {
				return iouring.New(cfg, &echoHandler{})
			},
		},
		{
			name: "epoll",
			newEng: func(cfg resource.Config) (engine.Engine, error) {
				return epoll.New(cfg, &echoHandler{})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resource.Config{
				Addr:              "127.0.0.1:0",
				Protocol:          engine.HTTP1,
				ReadHeaderTimeout: 2 * time.Second, // tight so the test is fast
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
				Logger:            nil,
			}
			eng, err := tc.newEng(cfg)
			if err != nil {
				if tc.minSkip != "" {
					t.Skip(tc.minSkip)
				}
				t.Skipf("%s engine unavailable on this host: %v", tc.name, err)
			}
			startEngine(t, eng)

			ag, ok := eng.(addrGetter)
			if !ok {
				t.Fatalf("%s engine has no Addr()", tc.name)
			}
			addr := ag.Addr().String()
			t.Logf("[%s] server listening on %s, ReadHeaderTimeout=%v", tc.name, addr, cfg.ReadHeaderTimeout)

			// Client phase.
			dial := net.Dialer{Timeout: 2 * time.Second}
			conn, err := dial.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()

			// Preamble: partial headers, no \r\n\r\n.
			preamble := []byte("GET / HTTP/1.1\r\nHost: test\r\nX-Drip: ")
			if _, err := conn.Write(preamble); err != nil {
				t.Fatalf("preamble write: %v", err)
			}
			t.Logf("[%s] preamble sent (%d bytes)", tc.name, len(preamble))

			startWall := time.Now()
			// Drip 'X' every 200ms for up to 12 * ReadHeaderTimeout — well
			// beyond the budget so a misfire is obvious.
			dripDeadline := time.After(12 * cfg.ReadHeaderTimeout)
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()

			var (
				closeObservedAt time.Time
				closeErr        error
				writeCount      int
			)
		drip:
			for {
				select {
				case <-dripDeadline:
					t.Logf("[%s] drip deadline reached after %v WITHOUT close — defence FAILED", tc.name, time.Since(startWall))
					break drip
				case <-ticker.C:
					writeCount++
					n, err := conn.Write([]byte("X"))
					elapsed := time.Since(startWall)
					if err != nil {
						closeObservedAt = time.Now()
						closeErr = err
						t.Logf("[%s] write #%d at %.2fs FAILED: %v (n=%d) — close observed", tc.name, writeCount, elapsed.Seconds(), err, n)
						break drip
					}
					if writeCount%5 == 0 {
						t.Logf("[%s] write #%d at %.2fs ok", tc.name, writeCount, elapsed.Seconds())
					}
				}
			}

			if closeObservedAt.IsZero() {
				t.Fatalf("[%s] DEFENCE FAILED: client wrote %d bytes over %v without close", tc.name, writeCount, time.Since(startWall))
			}
			closeLatency := closeObservedAt.Sub(startWall)
			slack := closeLatency - cfg.ReadHeaderTimeout
			t.Logf("[%s] DEFENCE FIRED at %.2fs (slack vs %v deadline: %v); err=%v",
				tc.name, closeLatency.Seconds(), cfg.ReadHeaderTimeout, slack, closeErr)

			// Assertion: close must fire within deadline + 2s slack.
			// This matches what the probatorium walker expects (12s budget
			// for a 10s deadline = 2s slack window).
			if slack > 2*time.Second {
				t.Errorf("[%s] close fired %v AFTER ReadHeaderTimeout — outside 2s slack window (probatorium walker would record a hang here)",
					tc.name, slack)
			}
		})
	}
}

// _ keeps the package compilable when celeris.New is the only consumer
// of these imports in the wider integration test surface.
var _ = fmt.Sprintf

// TestSlowlorisSyntheticConcurrent exercises the high-load condition
// probatorium hits: many simultaneous slowloris conns competing with
// non-slowloris background traffic. The synthetic single-client test
// (TestSlowlorisSynthetic) proves the defence works in isolation;
// THIS test probes whether it still fires reliably when the worker is
// busy with concurrent traffic — replicating what causes the 11%
// residual hang rate in the matrix nightly.
//
// Pattern:
//   - 1 server (per engine), short ReadHeaderTimeout (2s).
//   - N slowloris clients, all starting roughly simultaneously.
//   - M background clients doing fast successful requests.
//   - Each slowloris client measures its own close-observed-at time.
//   - Assertion: ALL N slowloris clients must observe close within
//     slack of ReadHeaderTimeout.
func TestSlowlorisSyntheticConcurrent(t *testing.T) {
	const (
		nSlowloris  = 64  // simultaneous slowloris attempts (probatorium fires many)
		nBackground = 200 // simultaneous fast clients (probatorium Markov walkers)
		readHdrTO   = 2 * time.Second
		slackBudget = 2 * time.Second // walker-style budget over the deadline
	)
	for _, tc := range []struct {
		name   string
		newEng func(cfg resource.Config) (engine.Engine, error)
	}{
		{
			name: "iouring",
			newEng: func(cfg resource.Config) (engine.Engine, error) {
				return iouring.New(cfg, &echoHandler{})
			},
		},
		{
			name: "epoll",
			newEng: func(cfg resource.Config) (engine.Engine, error) {
				return epoll.New(cfg, &echoHandler{})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resource.Config{
				Addr:              "127.0.0.1:0",
				Protocol:          engine.HTTP1,
				ReadHeaderTimeout: readHdrTO,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
				// Match probatorium refapp config: async dispatch pre-
				// allocates detachMu on every conn, forcing the close
				// path through finishCloseDetached (graceful close with
				// drain). This is the shape that produced 11% residual
				// hangs in the matrix nightly.
				AsyncHandlers: true,
			}
			eng, err := tc.newEng(cfg)
			if err != nil {
				t.Skipf("%s engine unavailable: %v", tc.name, err)
			}
			startEngine(t, eng)
			addr := eng.(addrGetter).Addr().String()
			t.Logf("[%s] server listening on %s", tc.name, addr)

			// Background traffic — simulates Markov walkers hammering the
			// server with successful requests. Drives the worker's CQE
			// rate high so the slowloris-conn timer SQE has to compete
			// for SQ ring space and CQE dispatch attention.
			bgDone := make(chan struct{})
			defer close(bgDone)
			for i := 0; i < nBackground; i++ {
				go func() {
					for {
						select {
						case <-bgDone:
							return
						default:
						}
						c, err := net.DialTimeout("tcp", addr, time.Second)
						if err != nil {
							time.Sleep(10 * time.Millisecond)
							continue
						}
						_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: bg\r\n\r\n"))
						_ = c.SetReadDeadline(time.Now().Add(time.Second))
						_, _ = c.Read(make([]byte, 256))
						_ = c.Close()
					}
				}()
			}
			// Let background traffic ramp up briefly so the worker is
			// already busy when the slowloris cohort starts.
			time.Sleep(200 * time.Millisecond)

			type result struct {
				idx             int
				closeObservedAt time.Duration
				err             error
				writesSucceeded int
			}
			results := make(chan result, nSlowloris)

			startBarrier := make(chan struct{})
			for i := 0; i < nSlowloris; i++ {
				go func(idx int) {
					<-startBarrier
					conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
					if err != nil {
						results <- result{idx: idx, err: err}
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: t\r\nX-Drip: "))
					start := time.Now()
					ticker := time.NewTicker(200 * time.Millisecond)
					defer ticker.Stop()
					budget := time.After(readHdrTO + slackBudget + time.Second)
					writes := 0
					for {
						select {
						case <-budget:
							results <- result{idx: idx, err: fmt.Errorf("DEFENCE-FAIL: budget exceeded, %d writes succeeded", writes), writesSucceeded: writes}
							return
						case <-ticker.C:
							writes++
							if _, err := conn.Write([]byte("X")); err != nil {
								results <- result{idx: idx, closeObservedAt: time.Since(start), writesSucceeded: writes}
								return
							}
						}
					}
				}(i)
			}
			close(startBarrier)

			var (
				ok, failed int
				maxClose   time.Duration
				latencies  []time.Duration
			)
			for i := 0; i < nSlowloris; i++ {
				r := <-results
				if r.err != nil {
					failed++
					t.Logf("[%s] client #%d FAILED: %v (writes=%d)", tc.name, r.idx, r.err, r.writesSucceeded)
					continue
				}
				ok++
				latencies = append(latencies, r.closeObservedAt)
				if r.closeObservedAt > maxClose {
					maxClose = r.closeObservedAt
				}
			}
			t.Logf("[%s] %d clients defended, %d failed; max close-latency=%v (deadline=%v slack=%v)",
				tc.name, ok, failed, maxClose, readHdrTO, slackBudget)
			if failed > 0 {
				t.Errorf("[%s] %d/%d clients NOT defended within %v slack", tc.name, failed, nSlowloris, slackBudget)
			}
			if maxClose > readHdrTO+slackBudget {
				t.Errorf("[%s] worst close-latency %v exceeded %v slack — probatorium walker would record a hang", tc.name, maxClose, slackBudget)
			}
		})
	}
}
