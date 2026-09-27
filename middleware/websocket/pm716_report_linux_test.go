//go:build linux

package websocket

// celeris#716 item 2 MEASUREMENT (throwaway branch tmp/b2b-716-stall; never merged): prints the
// pausedMu waits recorded in engineread.go for one subtest, then resets them.

import (
	"fmt"
	"math/bits"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func pm716Mode() string {
	if os.Getenv("CELERIS_WS716_ASYNC") == "1" {
		return "async"
	}
	return "sync"
}

func pm716Reset() {
	pm716.reqCalls.Store(0)
	pm716.hCalls.Store(0)
	pm716.engWait.take()
	pm716.engResume.take()
	pm716.hWait.take()
	pm716.hResume.take()
}

// pm716Dist renders a set of durations, padded with zeros up to total calls when total > len(v)
// (an uncontended acquisition waited 0).
func pm716Dist(v []int64, total int64) string {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	n := int64(len(v))
	if total < n {
		total = n
	}
	zeros := total - n
	at := func(q float64) int64 {
		if total == 0 {
			return 0
		}
		r := int64(q * float64(total-1))
		if r < zeros {
			return 0
		}
		return v[r-zeros]
	}
	var sum, ge1ms, ge10ms, ge100us int64
	for _, x := range v {
		sum += x
		if x >= int64(100*time.Microsecond) {
			ge100us++
		}
		if x >= int64(time.Millisecond) {
			ge1ms++
		}
		if x >= int64(10*time.Millisecond) {
			ge10ms++
		}
	}
	var mx int64
	if n > 0 {
		mx = v[n-1]
	}
	var hist [40]int64
	for _, x := range v {
		b := bits.Len64(uint64(x))
		if b >= len(hist) {
			b = len(hist) - 1
		}
		hist[b]++
	}
	var hs []string
	for b, c := range hist {
		if c > 0 {
			hs = append(hs, fmt.Sprintf("<%d:%d", int64(1)<<b, c))
		}
	}
	return fmt.Sprintf("calls=%d recorded=%d sumNs=%d maxNs=%d p50=%d p99=%d p999=%d p9999=%d ge100us=%d ge1ms=%d ge10ms=%d hist[%s]",
		total, n, sum, mx, at(0.5), at(0.99), at(0.999), at(0.9999), ge100us, ge1ms, ge10ms, strings.Join(hs, " "))
}

func pm716Report(t *testing.T, test, kind string, wall time.Duration) {
	req, hc := pm716.reqCalls.Load(), pm716.hCalls.Load()
	ew, er, hw, hr := pm716.engWait.take(), pm716.engResume.take(), pm716.hWait.take(), pm716.hResume.take()
	pre := fmt.Sprintf("PM716 test=%s kind=%s mode=%s wallNs=%d", test, kind, pm716Mode(), int64(wall))
	t.Logf("%s side=engine what=wait %s", pre, pm716Dist(ew, req))
	t.Logf("%s side=engine what=staleResumeCall %s", pre, pm716Dist(er, int64(len(er))))
	t.Logf("%s side=handler what=wait %s", pre, pm716Dist(hw, hc))
	t.Logf("%s side=handler what=resumeCall %s", pre, pm716Dist(hr, int64(len(hr))))
	pm716Reset()
}
