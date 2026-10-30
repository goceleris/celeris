//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// celeris#1170 (stage 3 of #889). A pool handler whose response the connection's
// outbound budget could not buffer waits for the peer's window in sendRest, and was
// bounded by an absolute WriteTimeout from the start of the wait (#906), whatever
// the client read. These cells use Go's own x/net/http2.Transport over a link of
// bps bytes/s, the client the issue was found with.

// slowConn1170 reads at most bps bytes per second in total (the link), 4 KiB a read.
type slowConn1170 struct {
	net.Conn
	th *throttled889
}

func (c *slowConn1170) Read(p []byte) (int, error) { return c.th.Read(p) }

// goTransport1170 is a Go HTTP/2 client over h2c (net/http's Transport with
// unencrypted HTTP/2, the bundled implementation of x/net/http2's transport: 4 MiB
// stream window, 1 GiB connection window) whose connection is read at bps.
func goTransport1170(bps float64) *http.Client {
	var pr http.Protocols
	pr.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: &pr,
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, a)
			if err != nil {
				return nil, err
			}
			sc := &slowConn1170{Conn: c}
			sc.th = &throttled889{r: c, bps: bps, t0: time.Now()}
			return sc, nil
		},
	}
	return &http.Client{Transport: tr}
}

// downloads1170 runs n parallel GETs of path (size bytes each) on one connection and
// reports, per request, how it ended and when.
func downloads1170(t *testing.T, addr, path string, n, size int, bps float64, limit time.Duration) (ends []string, at []time.Duration) {
	t.Helper()
	cl := goTransport1170(bps)
	// One tiny request first: the downloads share its connection (a Transport that
	// is asked for several at once may dial several).
	if resp, err := cl.Get("http://" + addr + path + "?n=1"); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return downloadsOn1170(t, cl, addr, path, n, size, limit)
}

// downloadsOn1170 is downloads1170 on a client that already holds its connection.
func downloadsOn1170(t *testing.T, cl *http.Client, addr, path string, n, size int, limit time.Duration) (ends []string, at []time.Duration) {
	t.Helper()
	ends, at = make([]string, n), make([]time.Duration, n)
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+path+"?n="+strconv.Itoa(size), nil)
			resp, err := cl.Do(req)
			if err != nil {
				ends[i], at[i] = "do: "+err.Error(), time.Since(t0)
				return
			}
			got, err := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			at[i] = time.Since(t0)
			switch {
			case err != nil:
				ends[i] = fmt.Sprintf("body after %d of %d: %v", got, size, err)
			case int(got) != size:
				ends[i] = fmt.Sprintf("short: %d of %d", got, size)
			default:
				ends[i] = "complete"
			}
		}()
	}
	wg.Wait()
	return ends, at
}

// TestH2GoTransportParallelDownloadsOnASlowLinkAreNotCut1170: four 8 MiB
// downloads by Go's Transport on a 1 MB/s link (each about 250 KB/s, four times the
// 64 KiB-per-window floor). On the previous head three of the four were reset with
// INTERNAL_ERROR (the client sees it at about 17 s, behind the bytes queued ahead of
// it): the first grant of the second, third and fourth stream comes 5, 9 and 14 s
// after their handlers began to wait (a 4 MiB stream window each is queued in the
// socket ahead of the next), and #906's deadline was 1 WriteTimeout.
//
// io_uring at WriteTimeout 1 s is not in the table: its own send clock (a send
// that does not complete for WriteTimeout, "io_uring is unchanged" in
// Config.WriteTimeout) closes the connection first, identically on main (measured:
// every stream ends in an unexpected EOF at 17 to 24 s). It is not this rule.
func TestH2GoTransportParallelDownloadsOnASlowLinkAreNotCut1170(t *testing.T) {
	// One WriteTimeout at a time, and at 1 s one engine at a time: a few of these cells
	// at once under -race or a coverage profile starve each other's event loops (CI saw
	// it), and a loop that is stuck in one batch for half a WriteTimeout switches the
	// rule off for the server (the fallback), which at 1 s is a cell's flake, not a
	// finding. At 5 s the threshold is 2.5 s and the engines run side by side.
	for _, wt := range []time.Duration{time.Second, 5 * time.Second} {
		t.Run(fmt.Sprintf("WT%v", wt), func(t *testing.T) {
			for _, e := range nativeEngines889() {
				if e.name == "io_uring" && wt < 5*time.Second {
					continue
				}
				t.Run(e.name, func(t *testing.T) {
					if wt >= 5*time.Second {
						t.Parallel()
					}
					var p pool1170
					addr := p.server(t, e.eng, -1, -1, wt)
					// Every loop has handled a batch of events before the download starts
					// and is idle after it: a loop that kept the stamp of its last batch
					// would read as stuck for ever, and switch the rule off.
					_ = get893(8, "http://"+addr+"/ping", 5*time.Second)
					time.Sleep(700 * time.Millisecond)
					ends, at := downloads1170(t, addr, "/pool", 4, 8<<20, 1e6, 60*time.Second)
					t.Logf("GT1170 %s/WT%v: %q at %v; %s", e.name, wt, ends, at, p.logs.summary())
					for i, r := range ends {
						if r != "complete" {
							t.Fatalf("download %d: %s at %v (all four must complete); %s", i, r, at[i].Round(time.Millisecond), p.logs.summary())
						}
					}
				})
			}
		})
	}
}

// TestH2GoTransportManyParallelDownloadsAreNotCut1170: eight 8 MiB downloads on a
// 2 MB/s link, WriteTimeout 1 s: the last stream's first grant is 7 x 4 MiB / 2 MB/s
// = 14 s after its wait began, 14 windows (the idle allowance covers it, not the 3 of
// the rule before celeris#1170).
func TestH2GoTransportManyParallelDownloadsAreNotCut1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		if e.name == "io_uring" {
			continue // its own send clock at WriteTimeout 1 s, see above
		}
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var p pool1170
			addr := p.server(t, e.eng, -1, -1, time.Second)
			ends, at := downloads1170(t, addr, "/pool", 8, 8<<20, 2e6, 90*time.Second)
			t.Logf("GT1170 many %s: %q at %v; %s", e.name, ends, at, p.logs.summary())
			for i, r := range ends {
				if r != "complete" {
					t.Fatalf("download %d: %s at %v (all eight must complete); %s", i, r, at[i].Round(time.Millisecond), p.logs.summary())
				}
			}
		})
	}
}

// TestH2GoTransportQueueBeyondTheCapIsCut1170: the cap exists. Four 8 MiB
// downloads on 1 MB/s at WriteTimeout 0.5 s: the last waiter's first grant is about
// 27 windows in, past the allowance (12 + the seed's 4), so it is reset, as
// documented ("N x W / L under about 20 x WriteTimeout").
func TestH2GoTransportQueueBeyondTheCapIsCut1170(t *testing.T) {
	for _, e := range nativeEngines889() {
		if e.name == "io_uring" {
			continue // its own send clock, see above
		}
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var p pool1170
			addr := p.server(t, e.eng, -1, -1, 500*time.Millisecond)
			ends, at := downloads1170(t, addr, "/pool", 4, 8<<20, 1e6, 60*time.Second)
			t.Logf("GT1170 beyond the cap %s: %q at %v; %s", e.name, ends, at, p.logs.summary())
			cut := 0
			for _, r := range ends {
				if r != "complete" {
					cut++
				}
			}
			if cut == 0 {
				t.Fatalf("all four completed: the queue (about 27 windows) is past the cap of 12 + 4 and a waiter must have been reset")
			}
		})
	}
}
