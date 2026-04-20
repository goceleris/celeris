package benchcmp_db

// Sustained-load tests. Opt-in via SUSTAINED_LOAD=1. Records per-request
// latency, sorts, reports p50/p90/p99/p999 + sustained RPS. Emits a
// markdown table.
//
// Run:
//   SUSTAINED_LOAD=1 SUSTAINED_DURATION=20s SUSTAINED_CONC=64 \
//     go test -run=TestSustained -timeout=15m -v ./test/benchcmp_db/

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sustainedResult struct {
	Framework string
	Ok        int64
	Err       int64
	Elapsed   time.Duration
	RPS       float64
	Pct       map[string]time.Duration // keys: p50, p90, p99, p999, max
}

func runSustained(t *testing.T, framework, url string, headers map[string]string, duration time.Duration, concurrency int) sustainedResult {
	t.Helper()
	samples := make([][]time.Duration, concurrency)
	perBucket := int(duration.Seconds())*15000/concurrency + 1024
	for i := range samples {
		samples[i] = make([]time.Duration, 0, perBucket)
	}
	var okCount, errCount atomic.Int64
	startCh := make(chan struct{})
	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	tr := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		MaxConnsPerHost:     concurrency * 2,
		IdleConnTimeout:     60 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	defer tr.CloseIdleConnections()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-startCh
			local := samples[w][:0]
			for {
				select {
				case <-stopCh:
					samples[w] = local
					return
				default:
				}
				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					errCount.Add(1)
					continue
				}
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				t0 := time.Now()
				resp, err := client.Do(req)
				elapsed := time.Since(t0)
				if err != nil {
					errCount.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					okCount.Add(1)
					local = append(local, elapsed)
				} else {
					errCount.Add(1)
				}
			}
		}(w)
	}

	t.Logf("warm-done; starting %s load...", framework)
	start := time.Now()
	close(startCh)
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()
	elapsed := time.Since(start)

	merged := make([]time.Duration, 0)
	for _, s := range samples {
		merged = append(merged, s...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })

	pct := func(frac float64) time.Duration {
		if len(merged) == 0 {
			return 0
		}
		idx := int(float64(len(merged)-1) * frac)
		return merged[idx]
	}
	res := sustainedResult{
		Framework: framework,
		Ok:        okCount.Load(),
		Err:       errCount.Load(),
		Elapsed:   elapsed,
		RPS:       float64(okCount.Load()) / elapsed.Seconds(),
		Pct: map[string]time.Duration{
			"p50":  pct(0.50),
			"p90":  pct(0.90),
			"p99":  pct(0.99),
			"p999": pct(0.999),
		},
	}
	if len(merged) > 0 {
		res.Pct["max"] = merged[len(merged)-1]
	}
	return res
}

func sustainedPreamble(t *testing.T) (time.Duration, int) {
	if os.Getenv("SUSTAINED_LOAD") != "1" {
		t.Skip("opt-in: set SUSTAINED_LOAD=1 to run (long)")
	}
	duration := 20 * time.Second
	if dv := os.Getenv("SUSTAINED_DURATION"); dv != "" {
		if d, err := time.ParseDuration(dv); err == nil {
			duration = d
		}
	}
	concurrency := 64
	if cv := os.Getenv("SUSTAINED_CONC"); cv != "" {
		fmt.Sscanf(cv, "%d", &concurrency)
	}
	return duration, concurrency
}

func printTable(t *testing.T, scenario string, rows []sustainedResult) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "\n## %s\n\n", scenario)
	fmt.Fprintf(&buf, "| Framework | ok | err | RPS | p50 | p90 | p99 | p999 | max |\n")
	fmt.Fprintf(&buf, "|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&buf, "| %s | %d | %d | %.0f | %v | %v | %v | %v | %v |\n",
			r.Framework, r.Ok, r.Err, r.RPS,
			r.Pct["p50"], r.Pct["p90"], r.Pct["p99"], r.Pct["p999"], r.Pct["max"])
	}
	t.Log(buf.String())
	if out := os.Getenv("SUSTAINED_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			_, _ = f.WriteString(buf.String())
			_ = f.Close()
		}
	}
}

func TestSustainedSession(t *testing.T) {
	duration, concurrency := sustainedPreamble(t)
	redisAddr := skipIfNoRedis(t)
	if err := primeSession(redisAddr); err != nil {
		t.Skipf("prime: %v", err)
	}

	type cfg struct {
		name  string
		start func(testing.TB, string) string
	}
	all := []cfg{
		{"Celeris", startCelerisSessionServer},
		{"Fiber", startFiberSessionServer},
		{"Echo", startEchoSessionServer},
		{"Chi", startChiSessionServer},
		{"Stdlib", startStdlibSessionServer},
	}
	var rows []sustainedResult
	for _, fw := range all {
		// Nested subtest so cleanup + independent skip works per framework.
		t.Run(fw.name, func(st *testing.T) {
			addr := fw.start(st, redisAddr)
			r := runSustained(st, fw.name, "http://"+addr+"/me",
				map[string]string{sessHeader: sessKey}, duration, concurrency)
			rows = append(rows, r)
		})
	}
	printTable(t, "Session via Redis (sustained)", rows)
}

func TestSustainedPGQuery(t *testing.T) {
	duration, concurrency := sustainedPreamble(t)
	dsn := skipIfNoPG(t)

	type cfg struct {
		name  string
		start func(testing.TB, string) string
	}
	all := []cfg{
		{"Celeris", startCelerisPGServer},
		{"Fiber", startFiberPGServer},
		{"Echo", startEchoPGServer},
		{"Chi", startChiPGServer},
		{"Stdlib", startStdlibPGServer},
	}
	var rows []sustainedResult
	for _, fw := range all {
		t.Run(fw.name, func(st *testing.T) {
			addr := fw.start(st, dsn)
			r := runSustained(st, fw.name, "http://"+addr+"/user",
				nil, duration, concurrency)
			rows = append(rows, r)
		})
	}
	printTable(t, "PGQuery (sustained)", rows)
}
