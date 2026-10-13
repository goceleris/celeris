//go:build linux

package stallprobe

// T10: the SIGURG lab. Data collection for golang/go#82146 (Cortex-A520 goroutine stalls during GC), not a
// pass/fail test: it tests rhysh's explanation, that a fast sender (A720) can send a follow-up SIGURG to a slow
// target (A520) while the target is still in the kernel for rt_sigreturn, so the kernel calls the handler again
// directly and the thread makes no progress toward the preemption request. Nothing is posted anywhere.
//
//	go test -run '^TestSigurgLab' -v     (celeris-stress: -f run='^TestSigurgLab')
//
// Four top-level tests, the same names on every host (the stress tally fails a run whose arches did not reach a
// verdict on the same tests, so nothing here t.Skips and nothing is a subtest): a host without a C compiler or
// without two core types logs LAB_SKIP / MATRIX_SKIP lines and the test PASSES.
//
//	TestSigurgLabUnit    fixed inputs for the parsers, statistics, plans (no process is started)
//	TestSigurgLabSmoke   short controls of the C program on this host: noack (every request must time out), a huge D
//	                     (send #2 completes, no re-entry), K=0 (send #1 completes), post-ack and pre-ack busy waits
//	TestSigurgLabC       experiment A, the plain-C protocol (testdata/sigurg_lab/sigurg_lab.c)
//	TestSigurgLabGo      experiment B, the Go reproducer (testdata/upstream/stallrepro.go) under four affinity arms
//
// Log prefixes: LAB_FACTS LAB_PLAN LAB_CONTROL LAB_BASE LAB_CELL LAB_TABLE LAB_ARM LAB_ARMTABLE LAB_BUDGET_STOP
// LAB_DONE LAB_SKIP, plus MATRIX_SKIP for a skip. Every line is one line; fake=true marks a fake class map.
//
// Knobs (celeris-stress extra=NAME=V, values [A-Za-z0-9_.,:/+-]):
//
//	CELERIS_PROBE_LAB_PARTS       comma list of C,GO (both)
//	CELERIS_PROBE_LAB_DS          D values in us for experiment A (0,1,2,5,10,20,50,100,200)
//	CELERIS_PROBE_LAB_REPS        reps per cell (3)
//	CELERIS_PROBE_LAB_SECS        seconds per cell (8)
//	CELERIS_PROBE_LAB_K           handler invocations per request that are refused (1)
//	CELERIS_PROBE_LAB_IDLE_US     idle between requests (2000)
//	CELERIS_PROBE_LAB_TMO_MS      per-request cap, counted as a timeout (1000)
//	CELERIS_PROBE_LAB_K0_SECS     seconds of the K=0 baseline cell per pair (4; 0 = none)
//	CELERIS_PROBE_LAB_C_BUDGET_MIN  minutes experiment A may take before it stops after a complete cell (18)
//	CELERIS_PROBE_LAB_GO_VERSION  toolchain of experiment B (1.27.2)
//	CELERIS_PROBE_LAB_GO_REPS     reps per arm (6)
//	CELERIS_PROBE_LAB_GO_SECS     seconds per run (30)
//	CELERIS_PROBE_LAB_GO_BUDGET_MIN  minutes experiment B may take (17)
//	CELERIS_PROBE_LAB_FAKECLASSES BIGS/LITTLES, e.g. 4-7/0-3: pretend a homogeneous host has two core types
//	                              (local validation only; every line says fake=true)

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/sigurg_lab/sigurg_lab.c
var labCSrc []byte

// ---- setup -----------------------------------------------------------------------------------------------------------

type labEnv struct {
	tmp, bin string
	tp       topo
	lt       labTopo
	fakeTag  string
}

func labSibs(cpu int) []int {
	s, err := parseCPUList(readTrim(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", cpu)))
	if err != nil {
		return nil
	}
	return s
}

func labDomain(tp topo) func(int) string {
	return func(c int) string { return tp.CPU[c].Domain }
}

func labHasPart(name string) bool {
	for _, p := range envList("CELERIS_PROBE_LAB_PARTS", "C,GO") {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

// labSetup makes the temp dir, loads the topology and compiles the C program. ok=false: a MATRIX_SKIP line was logged.
func labSetup(t *testing.T, name string, needCC bool) (e *labEnv, ok bool) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "siglab-")
	if err != nil {
		t.Logf("MATRIX_SKIP lab=%s reason=%q", name, "no temp dir: "+err.Error())
		return nil, false
	}
	t.Cleanup(func() {
		_ = filepath.Walk(tmp, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
		_ = os.RemoveAll(tmp)
	})
	tp := loadTopo()
	fake := os.Getenv("CELERIS_PROBE_LAB_FAKECLASSES")
	lt, err := labClasses(tp, fake)
	if err != nil {
		t.Logf("MATRIX_SKIP lab=%s reason=%q", name, err.Error())
		return nil, false
	}
	e = &labEnv{tmp: tmp, tp: tp, lt: lt}
	if lt.Fake {
		e.fakeTag = " fake=true"
	}
	if !needCC {
		return e, true
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		if cc, err = exec.LookPath("gcc"); err != nil {
			t.Logf("MATRIX_SKIP lab=%s reason=%q", name, "no C compiler (cc, gcc) on PATH")
			return nil, false
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "sigurg_lab.c"), labCSrc, 0o644); err != nil {
		t.Logf("MATRIX_SKIP lab=%s reason=%q", name, err.Error())
		return nil, false
	}
	e.bin = filepath.Join(tmp, "sigurg_lab")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, se, err := upRun(ctx, tmp, os.Environ(), cc, "-O2", "-pthread", "-o", e.bin, "sigurg_lab.c"); err != nil {
		t.Logf("MATRIX_SKIP lab=%s reason=%q", name, "compile failed: "+err.Error()+" "+tail(se, 600))
		return nil, false
	}
	return e, true
}

// runCell runs one cell of the C program and returns its summary and per-request records.
func (e *labEnv) runCell(label string, p labPair, d, secs int, extra ...string) (labCell, []labReq, error) {
	rec := filepath.Join(e.tmp, "rec-"+strconv.Itoa(int(time.Now().UnixNano()%1e9))+".txt")
	args := append([]string{"cell", strconv.Itoa(p.T), strconv.Itoa(p.S), strconv.Itoa(d), strconv.Itoa(secs), "out=" + rec, "label=" + label}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+45)*time.Second)
	defer cancel()
	out, se, err := upRun(ctx, e.tmp, os.Environ(), e.bin, args...)
	if err != nil {
		return labCell{}, nil, fmt.Errorf("%v: %s", err, tail(se, 300))
	}
	c, err := labParseCell(out)
	if err != nil {
		return c, nil, err
	}
	b, err := os.ReadFile(rec)
	_ = os.Remove(rec)
	if err != nil {
		return c, nil, err
	}
	reqs, err := labParseRecords(string(b))
	if err == nil && uint64(len(reqs)) != c.Requests {
		err = fmt.Errorf("record file has %d requests, LABCELL says %d", len(reqs), c.Requests)
	}
	return c, reqs, err
}

func (e *labEnv) logTopology(t *testing.T, name string) {
	t.Helper()
	t.Logf("LAB_FACTS lab=%s %s | go %s | classes: %s%s", name, hostFacts(), "n/a", e.lt.Note, e.fakeTag)
	t.Logf("LAB_FACTS topology: %s", e.tp.describe())
}

// ---- TestSigurgLabUnit ---------------------------------------------------------------------------------------------

func TestSigurgLabUnit(t *testing.T) {
	// records: request i (1..1000) took i us and needed one send, except: i%100==0 (100..900): 3 sends with one
	// re-entry; i=996..999: 60 ms storms (40 sends, 38 re-entries in a row); i=1000: a timeout (1 s, 90000 sends)
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		lat, sends, hand, re, zd, mc, to := int64(i)*1000, 1, 1, 0, 0, 0, 0
		switch {
		case i == 1000:
			lat, sends, hand, re, zd, mc, to = 1_000_000_000, 90000, 90000, 89999, 89999, 12000, 1
		case i >= 996:
			lat, sends, hand, re, zd, mc = 60_000_000, 40, 40, 38, 38, 38
		case i%100 == 0:
			sends, hand, re, zd, mc = 3, 3, 1, 1, 1
		}
		fmt.Fprintf(&sb, "%d %d %d %d %d %d %d\n", lat, sends, hand, re, zd, mc, to)
	}
	reqs, err := labParseRecords(sb.String())
	if err != nil || len(reqs) != 1000 {
		t.Fatalf("labParseRecords: %d requests, err %v", len(reqs), err)
	}
	st := labSummarize(reqs)
	// sorted: 1..995 us, four 60 ms, one 1 s. percentiles() reads s[int(q*(n-1))]: indexes 499, 989, 998
	if st.P50 != 500_000 || st.P99 != 990_000 || st.P999 != 60_000_000 || st.Max != 1_000_000_000 {
		t.Errorf("percentiles: p50=%d p99=%d p999=%d max=%d, want 500000 990000 60000000 1000000000", st.P50, st.P99, st.P999, st.Max)
	}
	if st.Over1ms != 0.005 || st.Over50ms != 0.005 { // the four storms and the timeout; a request of exactly 1 ms is not over 1 ms
		t.Errorf("Over1ms=%v Over50ms=%v, want 0.005 0.005", st.Over1ms, st.Over50ms)
	}
	if st.Timeouts != 1 || st.SendsMax != 90000 || st.MaxConsec != 12000 || st.MultiSend != 9+4+1 {
		t.Errorf("timeouts=%d sends_max=%d max_consec=%d multi_send=%d, want 1 90000 12000 14", st.Timeouts, st.SendsMax, st.MaxConsec, st.MultiSend)
	}
	if st.ReqWithReentry != 9+4+1 || st.ReqOverConsec10 != 4+1 {
		t.Errorf("req_with_reentry=%d req_consec10=%d, want 14 and 5", st.ReqWithReentry, st.ReqOverConsec10)
	}
	if want := int64(9*1 + 4*38 + 89999); st.Reentry != want {
		t.Errorf("reentry total %d, want %d", st.Reentry, want)
	}
	for _, bad := range []string{"1 2 3\n", "1 2 3 4 5 6 x\n", "1 2 3 4 5 6 -7\n"} {
		if _, err := labParseRecords(bad); err == nil {
			t.Errorf("labParseRecords(%q) accepted a malformed record", bad)
		}
	}

	// histograms: the bucket edges must be the C program's (hidx)
	for ns, want := range map[int64]int{0: 0, 999: 0, 1000: 1, 1100: 1, 1200: 2, 1500: 3, 1700: 4, 2000: 5, 5000: 10, 22000: 18, 1_000_000: 40, 1 << 62: 127} {
		if got := labBucket(ns); got != want {
			t.Errorf("labBucket(%d) = %d, want %d", ns, got, want)
		}
	}
	h, err := labParseHist("0:5,3:7,40:1")
	if err != nil || h.total() != 13 {
		t.Fatalf("labParseHist: %v total %d", err, h.total())
	}
	if q := h.quantile(0.5); q < 1400 || q > 1700 { // 7th of 13 sits in bucket 3 = [1414,1682) ns
		t.Errorf("quantile(0.5) = %v, want inside bucket 3 [1414, 1682)", q)
	}
	if q := h.quantile(0.01); q != 500 {
		t.Errorf("quantile(0.01) = %v, want 500 (bucket 0)", q)
	}
	if h0, _ := labParseHist("-"); h0.quantile(0.5) != -1 {
		t.Errorf("empty histogram quantile must be -1")
	}
	for _, bad := range []string{"3", "a:1", "200:1", "1:x"} {
		if _, err := labParseHist(bad); err == nil {
			t.Errorf("labParseHist(%q) accepted a malformed histogram", bad)
		}
	}

	// the LABCELL line
	line := "noise\nLABCELL label=x t=2 s=0 d_us=5 k=1 requests=3 completes=2 timeouts=1 sends=9 sends_max=5 resends=6 resends_waited=2 handlers=9 reentry=4 zero_delta=4 maxconsec=3 ack_hist=0:1,9:2 gap_hist=- a2r_hist=5:1\n"
	c, err := labParseCell(line)
	if err != nil || c.Requests != 3 || c.Timeout != 1 || c.Reentry != 4 || c.Ack.total() != 3 || c.Gap.total() != 0 || c.KV["label"] != "x" {
		t.Errorf("labParseCell: %+v err %v", c, err)
	}
	if _, err := labParseCell("LABCELL requests=1"); err == nil {
		t.Errorf("labParseCell accepted a line without the counters")
	}
	if _, err := labParseCell("nothing"); err == nil {
		t.Errorf("labParseCell accepted output without a LABCELL line")
	}
	pool := newLabPool()
	pool.add(c, reqs[:10])
	pool.add(c, reqs[10:20])
	if pool.reps != 2 || len(pool.reqs) != 20 || pool.ack.total() != 6 || pool.reentry != 8 || pool.resends != 12 || pool.waited != 4 {
		t.Errorf("pool: %+v", pool)
	}
	if l := labStatsLine(labSummarize(pool.reqs), pool.ack, pool.gap, pool.a2r, pool.resends, pool.waited); !strings.Contains(l, "resends_waited_frac=0.333") || !strings.Contains(l, "requests=20") {
		t.Errorf("labStatsLine: %s", l)
	}

	// classes, pairs, plan
	hetTp := topo{Allowed: []int{0, 1, 2, 3, 4, 5, 6, 7}, Hetero: true, CPU: map[int]cpuInfo{}, Classes: [][]int{{0, 1, 6, 7}, {2, 3, 4, 5}}}
	for _, c := range []int{0, 1, 6, 7} {
		hetTp.CPU[c] = cpuInfo{ID: c, Cap: 1024 - c, Domain: map[int]string{0: "0,1", 1: "0,1", 6: "6,7", 7: "6,7"}[c]}
	}
	for _, c := range []int{2, 3, 4, 5} {
		hetTp.CPU[c] = cpuInfo{ID: c, Cap: 279, Domain: "2-5"}
	}
	lt, err := labClasses(hetTp, "")
	if err != nil || !lt.Hetero || lt.Fake || fmtCPUs(lt.Bigs) != "0-1,6-7" || fmtCPUs(lt.Littles) != "2-5" || lt.Fast != 0 {
		t.Fatalf("labClasses(hetero): %+v err %v", lt, err)
	}
	pairs := labPairs(lt, labDomain(hetTp), func(int) []int { return nil })
	var names []string
	for _, p := range pairs {
		names = append(names, fmt.Sprintf("%s:%d<-%d", p.Name, p.T, p.S))
	}
	if got, want := strings.Join(names, " "), "little<-big:2<-0 little<-little:2<-3 big<-big:0<-1 big<-little:0<-2"; got != want {
		t.Errorf("labPairs = %s, want %s", got, want)
	}
	hom := topo{Allowed: []int{0, 1, 2, 3}, CPU: map[int]cpuInfo{}, Classes: [][]int{{0, 1, 2, 3}}}
	ltH, _ := labClasses(hom, "")
	pH := labPairs(ltH, labDomain(hom), func(c int) []int { return []int{c, c ^ 1} }) // SMT pairs 0/1 and 2/3
	if ltH.Hetero || len(pH) != 1 || pH[0].T != 0 || pH[0].S != 2 || !pH[0].ControlOnly {
		t.Errorf("homogeneous pairs: %+v (sender must skip the SMT sibling)", pH)
	}
	if lf, err := labClasses(hom, "2-3/0-1"); err != nil || !lf.Fake || !lf.Hetero || fmtCPUs(lf.Bigs) != "2-3" || fmtCPUs(lf.Littles) != "0-1" {
		t.Errorf("fake map: %+v err %v", lf, err)
	}
	for _, bad := range []string{"2-3", "2-3/", "2-3/3-4", "2-9/0-1", "a/b"} {
		if _, err := labClasses(hom, bad); err == nil {
			t.Errorf("labClasses(fake %q) accepted a bad map", bad)
		}
	}
	ds, err := labParseDs([]string{"0", "1", "2", "5", "10", "20", "50", "100", "200"})
	if err != nil || len(ds) != 9 {
		t.Fatalf("labParseDs: %v %v", ds, err)
	}
	if _, err := labParseDs([]string{"-1"}); err == nil {
		t.Errorf("labParseDs accepted -1")
	}
	plan := labPlan(pairs, ds, 3)
	if len(plan) != 3*4*9 {
		t.Errorf("plan has %d cells, want 108", len(plan))
	}
	seen := map[string]int{}
	for _, c := range plan {
		seen[fmt.Sprintf("%d/%s/%d", c.Rep, c.Pair.Name, c.D)]++
	}
	if len(seen) != 108 {
		t.Errorf("plan repeats a cell: %d distinct of 108", len(seen))
	}
	if plan[0].Rep != 1 || plan[len(plan)-1].Rep != 3 || plan[0].D != 0 || plan[36].Rep != 2 || plan[36].D != 10 {
		t.Errorf("plan order: first %+v, second rep starts with %+v (want rep 2, D=10: rotated by 4)", plan[0], plan[36])
	}
	if got := float64(len(plan)) * labCellSeconds(8) / 60; got < 14 || got > 15.5 {
		t.Errorf("experiment A budget %.2f min, expected about 14.9", got)
	}

	// arms
	arms := labArms(lt, hetTp)
	if len(arms) != 5 || arms[0].Pin != nil || arms[0].Cpus != nil || fmtCPUs(arms[1].Pin) != "2-5" || fmtCPUs(arms[1].Cpus) != "2-5" ||
		fmtCPUs(arms[2].Pin) != "0-1,6-7" || fmtCPUs(arms[2].Cpus) != "2-5" || fmtCPUs(arms[3].Pin) != "2-5" || len(arms[3].Cpus) != 4 || !arms[4].NoAsync {
		t.Errorf("arms: %+v", arms)
	}
	for _, a := range arms[3].Cpus {
		if a == 2 || a == 3 || a == 4 || a == 5 {
			t.Errorf("reverse arm puts a loop on little cpu %d", a)
		}
	}
	agg := labArmSummary([]labArmRow{{"a", 1, 100, "STALL"}, {"a", 2, 8, "OK"}, {"a", 3, 300, "STALL"}, {"a", 4, 0, "HANG"}, {"b", 1, 8, "OK"}, {"b", 2, 9, "OK"}, {"b", 3, 0, "ERROR"}})
	if len(agg) != 2 || agg[0].Runs != 4 || agg[0].Stalled != 3 || agg[0].MaxMs != 300 || agg[0].MedianMs != 100 || agg[0].MedStallMs != 100 ||
		agg[1].Runs != 3 || agg[1].Stalled != 0 || agg[1].MedianMs != 8.5 || agg[1].MedStallMs != -1 {
		t.Errorf("labArmSummary: %+v", agg)
	}

	// /proc readers
	after := make([]string, 50) // the fields after comm; after[0] is field 3 (state), so field 39 is after[36]
	for i := range after {
		after[i] = "0"
	}
	after[0], after[36] = "S", "11"
	stat := "1234 (Stall re) pro (x)) " + strings.Join(after, " ")
	if got := labStatLastCPU(stat); got != 11 {
		t.Errorf("labStatLastCPU = %d, want 11", got)
	}
	if labStatLastCPU("garbage") != -1 || labStatLastCPU("1 (a) S 1 2") != -1 {
		t.Errorf("labStatLastCPU accepted a short line")
	}
	if got := labStatusMask("Name:\tx\nCpus_allowed:\tff\nCpus_allowed_list:\t0-7\n"); got != "0-7" || labStatusMask("Name:\tx\n") != "?" {
		t.Errorf("labStatusMask = %q", got)
	}
	if sum := labProcSummary(os.Getpid(), []int{0}, []int{1}); !strings.HasPrefix(sum, "threads=") {
		t.Errorf("labProcSummary of this process: %s", sum)
	}

	// the reproducer patch
	patched, hook, err := labPatchRepro(upReproSrc)
	if err != nil || strings.Count(string(patched), "labCalib(fast)") != 1 || !strings.Contains(string(hook), "STALLREPRO_CALIB_CPU") {
		t.Errorf("labPatchRepro: %v", err)
	}
	if _, _, err := labPatchRepro([]byte("package main\n")); err == nil {
		t.Errorf("labPatchRepro accepted a source without the anchor")
	}
	t.Logf("LAB_UNIT ok: plan %d cells, stats p50=%d p99=%d p999=%d max=%d", len(plan), st.P50, st.P99, st.P999, st.Max)
}

// ---- TestSigurgLabSmoke ------------------------------------------------------------------------------------------------

// TestSigurgLabSmoke runs the C program's controls on this host. noack, huge D and K=0 are deterministic by
// construction and are asserted; the post-ack and pre-ack busy waits are timing dependent and only logged.
func TestSigurgLabSmoke(t *testing.T) {
	e, ok := labSetup(t, "smoke", true)
	if !ok {
		return
	}
	e.logTopology(t, "smoke")
	pairs := labPairs(e.lt, labDomain(e.tp), labSibs)
	if len(pairs) == 0 {
		t.Logf("MATRIX_SKIP lab=smoke reason=%q", "fewer than 2 allowed CPUs")
		return
	}
	p := pairs[0]
	for _, q := range pairs { // the control cells use the pair whose sender is the fastest class we have: first same-class pair
		if q.SameClass || q.ControlOnly {
			p = q
			break
		}
	}
	ctl := func(name string, d, secs int, extra ...string) (labCell, labStats) {
		c, reqs, err := e.runCell("ctl-"+name, p, d, secs, extra...)
		if err != nil {
			t.Errorf("LAB_CONTROL %s: %v", name, err)
			return c, labStats{}
		}
		st := labSummarize(reqs)
		t.Logf("LAB_CONTROL name=%s pair=%s t=%d s=%d d_us=%d extra=%q %s%s", name, p.Name, p.T, p.S, d, strings.Join(extra, " "),
			labStatsLine(st, c.Ack, c.Gap, c.A2R, c.Resends, c.Waited), e.fakeTag)
		return c, st
	}
	// negative control: a handler that never acknowledges. signalPending stays 1, no resend, every request times out.
	c, st := ctl("noack", 5, 2, "mode=noack", "tmo_ms=200")
	if st.Requests == 0 || c.Completes != 0 || int(c.Timeout) != st.Requests || st.Timeouts != st.Requests {
		t.Errorf("NEGATIVE CONTROL noack: requests=%d completes=%d timeouts=%d, want every request to time out", st.Requests, c.Completes, c.Timeout)
	}
	// huge D: the first handler is refused (K=1), the resend comes 20 ms later, the thread has run in user mode: send #2 completes, no re-entry
	c, st = ctl("hugeD", 20000, 2)
	if st.Requests < 5 || st.Timeouts != 0 || c.Sends != 2*c.Requests || st.Reentry != 0 || st.P50 < 19_000_000 {
		t.Errorf("NEGATIVE CONTROL D=20ms: requests=%d timeouts=%d sends=%d reentry=%d p50=%d, want every request done on send #2 without re-entry", st.Requests, st.Timeouts, c.Sends, st.Reentry, st.P50)
	}
	// K=0: the first handler already saw user progress: send #1 completes
	c, st = ctl("k0", 5, 1, "k=0")
	if st.Requests < 50 || st.Timeouts != 0 || c.Sends != c.Requests || st.Reentry != 0 {
		t.Errorf("CONTROL K=0: requests=%d timeouts=%d sends=%d reentry=%d, want every request done on send #1", st.Requests, st.Timeouts, c.Sends, st.Reentry)
	}
	// baseline and the two busy waits (logged, not asserted)
	_, base := ctl("base", 5, 2)
	_, post := ctl("postack50us", 5, 2, "postack_ns=50000")
	_, pre := ctl("preack50us", 5, 2, "preack_ns=50000")
	t.Logf("LAB_CONTROL summary: reentry base=%d post-ack-50us=%d pre-ack-50us=%d (expected: post-ack >> pre-ack; post-ack > 0 shows the harness detects re-entry) timeouts base=%d post=%d pre=%d%s",
		base.Reentry, post.Reentry, pre.Reentry, base.Timeouts, post.Timeouts, pre.Timeouts, e.fakeTag)
}

// ---- TestSigurgLabC: experiment A -------------------------------------------------------------------------------------

// TestSigurgLabC runs the plain-C protocol (sigurg_lab.c) over the pairs x D x reps plan.
//
// Time budget (arm64 heterogeneous, defaults): 4 pairs x 9 D x 3 reps = 108 cells x (8 s + 0.3 s start/stop) = 14.9 min,
// plus 4 pair facts (~2 s each), 4 K=0 baselines (4 s each) and the controls of TestSigurgLabSmoke (~12 s): about 15.5 min.
// A homogeneous host (x86) runs the one same-class pair: 27 cells = 3.7 min plus facts and baseline: about 4 min.
func TestSigurgLabC(t *testing.T) {
	if !labHasPart("C") {
		t.Logf("LAB_SKIP lab=C reason=%q", "not in CELERIS_PROBE_LAB_PARTS")
		return
	}
	e, ok := labSetup(t, "C", true)
	if !ok {
		return
	}
	ds, err := labParseDs(envList("CELERIS_PROBE_LAB_DS", "0,1,2,5,10,20,50,100,200"))
	if err != nil {
		t.Logf("MATRIX_SKIP lab=C reason=%q", err.Error())
		return
	}
	reps, secs := envInt("CELERIS_PROBE_LAB_REPS", 3), envInt("CELERIS_PROBE_LAB_SECS", 8)
	k, idle, tmo := envInt("CELERIS_PROBE_LAB_K", 1), envInt("CELERIS_PROBE_LAB_IDLE_US", 2000), envInt("CELERIS_PROBE_LAB_TMO_MS", 1000)
	k0secs, budget := envInt("CELERIS_PROBE_LAB_K0_SECS", 4), time.Duration(envInt("CELERIS_PROBE_LAB_C_BUDGET_MIN", 18))*time.Minute
	e.logTopology(t, "C")
	pairs := labPairs(e.lt, labDomain(e.tp), labSibs)
	if len(pairs) == 0 {
		t.Logf("MATRIX_SKIP lab=C reason=%q", "fewer than 2 allowed CPUs")
		return
	}
	plan := labPlan(pairs, ds, reps)
	var pn []string
	for _, p := range pairs {
		pn = append(pn, fmt.Sprintf("%s(t=%d[%s] s=%d[%s] domain_t=%q domain_s=%q)", p.Name, p.T, e.tp.CPU[p.T].class(), p.S, e.tp.CPU[p.S].class(), e.tp.CPU[p.T].Domain, e.tp.CPU[p.S].Domain))
	}
	t.Logf("LAB_PLAN lab=C pairs=%s ds=%v reps=%d secs=%d k=%d idle_us=%d tmo_ms=%d cells=%d est_min=%.1f budget_min=%.0f%s",
		strings.Join(pn, " "), ds, reps, secs, k, idle, tmo, len(plan), (float64(len(plan))*labCellSeconds(secs)+float64(len(pairs))*(2+float64(k0secs)+1))/60, budget.Minutes(), e.fakeTag)
	start := time.Now()
	common := []string{"k=" + strconv.Itoa(k), "idle_us=" + strconv.Itoa(idle), "tmo_ms=" + strconv.Itoa(tmo)}

	// facts per pair: kernel, MIDR when readable, capacity, frequency, ns/iter, self and cross signal path timing
	for _, p := range pairs {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, se, err := upRun(ctx, e.tmp, os.Environ(), e.bin, "facts", strconv.Itoa(p.T), strconv.Itoa(p.S), "10000")
		cancel()
		if err != nil {
			t.Logf("LAB_SKIP lab=C what=facts pair=%s reason=%q", p.Name, err.Error()+" "+tail(se, 200))
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			t.Logf("LAB_FACTS pair=%s tcls=%s scls=%s %s%s", p.Name, p.TCls, p.SCls, strings.TrimPrefix(l, "LABFACTS "), e.fakeTag)
		}
	}
	// K=0 baseline: the literal "first handler with progress completes" rule, every request done on send #1
	if k0secs > 0 {
		for _, p := range pairs {
			c, reqs, err := e.runCell("base-k0", p, 5, k0secs, "k=0", "idle_us="+strconv.Itoa(idle), "tmo_ms="+strconv.Itoa(tmo))
			if err != nil {
				t.Logf("LAB_SKIP lab=C what=base-k0 pair=%s reason=%q", p.Name, err.Error())
				continue
			}
			t.Logf("LAB_BASE name=k0 pair=%s t=%d s=%d d_us=5 %s%s", p.Name, p.T, p.S, labStatsLine(labSummarize(reqs), c.Ack, c.Gap, c.A2R, c.Resends, c.Waited), e.fakeTag)
		}
	}

	pools := map[string]*labPool{}
	key := func(p labPair, d int) string { return fmt.Sprintf("%s/%d", p.Name, d) }
	done := 0
	for _, c := range plan {
		if time.Since(start)+time.Duration(labCellSeconds(secs)*float64(time.Second)) > budget {
			t.Logf("LAB_BUDGET_STOP lab=C cells_done=%d of %d elapsed_min=%.1f budget_min=%.0f%s", done, len(plan), time.Since(start).Minutes(), budget.Minutes(), e.fakeTag)
			break
		}
		cell, reqs, err := e.runCell("matrix", c.Pair, c.D, secs, common...)
		if err != nil {
			t.Logf("LAB_SKIP lab=C what=cell rep=%d pair=%s d_us=%d reason=%q", c.Rep, c.Pair.Name, c.D, err.Error())
			continue
		}
		done++
		pl := pools[key(c.Pair, c.D)]
		if pl == nil {
			pl = newLabPool()
			pools[key(c.Pair, c.D)] = pl
		}
		pl.add(cell, reqs)
		t.Logf("LAB_CELL rep=%d pair=%s t=%d s=%d tcls=%s scls=%s d_us=%d ns_per_iter=%s %s%s", c.Rep, c.Pair.Name, c.Pair.T, c.Pair.S, c.Pair.TCls, c.Pair.SCls, c.D,
			cell.KV["ns_per_iter"], labStatsLine(labSummarize(reqs), cell.Ack, cell.Gap, cell.A2R, cell.Resends, cell.Waited), e.fakeTag)
	}
	for _, p := range pairs {
		for _, d := range ds {
			pl := pools[key(p, d)]
			if pl == nil {
				continue
			}
			t.Logf("LAB_TABLE pair=%s t=%d s=%d tcls=%s scls=%s d_us=%d reps=%d %s%s", p.Name, p.T, p.S, p.TCls, p.SCls, d, pl.reps,
				labStatsLine(labSummarize(pl.reqs), pl.ack, pl.gap, pl.a2r, pl.resends, pl.waited), e.fakeTag)
		}
	}
	t.Logf("LAB_DONE lab=C cells_done=%d of %d elapsed_min=%.1f%s", done, len(plan), time.Since(start).Minutes(), e.fakeTag)
}

// ---- TestSigurgLabGo: experiment B --------------------------------------------------------------------------------------

// labRunRepro runs the reproducer once like upRun, but with Start/Wait so that the threads of the live process can
// be read mid-run: the proof that the arm's affinity did what it claims.
func labRunRepro(secs int, env []string, dir, name string, args []string, littles, bigs []int) (stdout, stderr, proc string, hung bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+30)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err = cmd.Start(); err != nil {
		return "", err.Error(), "", false, err
	}
	pid := cmd.Process.Pid
	sample := make(chan string, 1)
	tm := time.AfterFunc(time.Duration(secs)*time.Second/2+time.Second, func() { sample <- labProcSummary(pid, littles, bigs) })
	err = cmd.Wait()
	tm.Stop()
	select {
	case proc = <-sample:
	default:
		proc = "proc=not-sampled"
	}
	return so.String(), se.String(), proc, ctx.Err() != nil, err
}

// TestSigurgLabGo runs the Go reproducer under process-affinity arms (one toolchain, rep outer, arms interleaved).
//
// Time budget (arm64 heterogeneous, defaults): 5 arms x 6 reps x 31 s = 15.5 min plus the toolchain download and two builds
// (~1.5 min). A homogeneous host logs MATRIX_SKIP: it has no sender class to vary and TestUpstreamMatrix already covers
// its default arm.
func TestSigurgLabGo(t *testing.T) {
	if !labHasPart("GO") {
		t.Logf("LAB_SKIP lab=GO reason=%q", "not in CELERIS_PROBE_LAB_PARTS")
		return
	}
	e, ok := labSetup(t, "GO", true)
	if !ok {
		return
	}
	e.logTopology(t, "GO")
	if !e.lt.Hetero {
		t.Logf("MATRIX_SKIP lab=GO reason=%q", "no heterogeneous cores on this host: sender class cannot be varied")
		return
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Logf("MATRIX_SKIP lab=GO reason=%q", "no go command on PATH: "+err.Error())
		return
	}
	ver := envList("CELERIS_PROBE_LAB_GO_VERSION", "1.27.2")[0]
	reps, secs := envInt("CELERIS_PROBE_LAB_GO_REPS", 6), envInt("CELERIS_PROBE_LAB_GO_SECS", 30)
	budget := time.Duration(envInt("CELERIS_PROBE_LAB_GO_BUDGET_MIN", 17)) * time.Minute
	start := time.Now()
	arms := labArms(e.lt, e.tp)
	var armNames []string
	for _, a := range arms {
		armNames = append(armNames, fmt.Sprintf("%s[pin=%s loops=%s]", a.Name, fmtCPUs(a.Pin), fmtCPUs(a.Cpus)))
	}
	t.Logf("LAB_PLAN lab=GO version=%s reps=%d secs=%d arms=%s est_min=%.1f budget_min=%.0f fast_cpu=%d%s", ver, reps, secs, strings.Join(armNames, " "),
		float64(len(arms)*reps*(secs+1))/60+1.5, budget.Minutes(), e.lt.Fast, e.fakeTag)

	// two builds: the original reproducer (control arm) and the lab copy (calibration hook, see labPatchRepro)
	ue := upEnv{tmp: e.tmp, toolchain: "go" + strings.TrimPrefix(ver, "go")}
	for _, d := range []string{"gopath", "gocache", "gomodcache", "home", "xdg"} {
		_ = os.MkdirAll(filepath.Join(e.tmp, d), 0o755)
	}
	build := func(dir string, files map[string][]byte) (string, bool) {
		_ = os.MkdirAll(filepath.Join(e.tmp, dir), 0o755)
		for n, b := range files {
			_ = os.WriteFile(filepath.Join(e.tmp, dir, n), b, 0o644)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		dd := filepath.Join(e.tmp, dir)
		gv, se, err := upRun(ctx, dd, ue.env(), goBin, "version")
		m := regexp.MustCompile(`go version go(\d+\.\d+)`).FindStringSubmatch(gv)
		if err != nil || m == nil {
			t.Logf("MATRIX_SKIP lab=GO reason=%q", "toolchain go"+ver+" unavailable: "+fmt.Sprint(err)+" "+tail(se, 300))
			return "", false
		}
		_ = os.WriteFile(filepath.Join(dd, "go.mod"), []byte("module stallrepro\n\ngo "+m[1]+"\n"), 0o644)
		out := filepath.Join(e.tmp, "bin-"+dir)
		if _, se, err = upRun(ctx, dd, ue.env(), goBin, "build", "-o", out, "."); err != nil {
			t.Logf("MATRIX_SKIP lab=GO reason=%q", "build failed: "+err.Error()+" "+tail(se, 500))
			return "", false
		}
		t.Logf("LAB_FACTS lab=GO built %s with %s%s", dir, strings.TrimSpace(gv), e.fakeTag)
		return out, true
	}
	patched, hook, err := labPatchRepro(upReproSrc)
	if err != nil {
		t.Logf("MATRIX_SKIP lab=GO reason=%q", err.Error())
		return
	}
	orig, ok1 := build("orig", map[string][]byte{"main.go": upReproSrc})
	lab, ok2 := build("lab", map[string][]byte{"main.go": patched, "labcalib.go": hook})
	if !ok1 || !ok2 {
		return
	}
	t.Logf("LAB_FACTS lab=GO stallrepro.go sha256 %x (original); the lab copy differs only in affinity(fast) -> affinity(labCalib(fast)) plus labcalib.go (STALLREPRO_CALIB_CPU)%s", sha256.Sum256(upReproSrc), e.fakeTag)

	// how to start a process with a mask: the C helper (sched_setaffinity, then execvp), else taskset
	pinner := func(mask []int, argv []string) (string, []string, bool) {
		if e.bin != "" {
			return e.bin, append([]string{"exec", fmtCPUs(mask)}, argv...), true
		}
		if ts, err := exec.LookPath("taskset"); err == nil {
			return ts, append([]string{"-c", fmtCPUs(mask)}, argv...), true
		}
		return "", nil, false
	}
	var rows []labArmRow
	stopped := false
	for rep := 1; rep <= reps && !stopped; rep++ {
		for _, a := range arms {
			if time.Since(start)+time.Duration(secs+3)*time.Second > budget {
				t.Logf("LAB_BUDGET_STOP lab=GO runs_done=%d elapsed_min=%.1f budget_min=%.0f%s", len(rows), time.Since(start).Minutes(), budget.Minutes(), e.fakeTag)
				stopped = true
				break
			}
			bin := orig
			env := ue.env()
			if a.Lab {
				bin = lab
				env = append(env, "STALLREPRO_CALIB_CPU="+strconv.Itoa(e.lt.Fast))
			}
			if a.NoAsync {
				env = append(env, "GODEBUG=asyncpreemptoff=1")
			}
			flags := []string{"-secs=" + strconv.Itoa(secs), "-threshold-ms=50"}
			if a.Cpus == nil {
				flags = append(flags, "-class=little")
			} else {
				flags = append(flags, "-cpus="+fmtCPUs(a.Cpus))
			}
			name, args := bin, flags
			if a.Pin != nil {
				var okp bool
				if name, args, okp = pinner(a.Pin, append([]string{bin}, flags...)); !okp {
					t.Logf("MATRIX_SKIP lab=GO arm=%s reason=%q", a.Name, "neither the C helper nor taskset can set the process mask")
					continue
				}
			}
			out, se, proc, hung, err := labRunRepro(secs, env, e.tmp, name, args, e.lt.Littles, e.lt.Bigs)
			kv := map[string]string{}
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "RESULT ") {
					kv = parseKV(l)
				}
			}
			verdict := map[string]string{"PASS": "OK", "STALL": "STALL"}[kv["verdict"]]
			switch {
			case hung:
				verdict = "HANG"
			case verdict == "":
				verdict = "ERROR"
			}
			gap, _ := strconv.ParseFloat(kv["max_gap_ms"], 64)
			extra := ""
			if verdict == "HANG" || verdict == "ERROR" {
				extra = fmt.Sprintf(" err=%v stderr=%q", err, tail(se, 300))
			}
			t.Logf("LAB_ARM arm=%s rep=%d max_gap_ms=%s gc_cycles=%s verdict=%s gomaxprocs=%s work_iters=%s ns_per_iter=%s tick_gap_ms=%s cpus=%s pin=%s %s%s%s",
				a.Name, rep, kv["max_gap_ms"], kv["gc_cycles"], verdict, kv["gomaxprocs"], kv["work_iters"], kv["ns_per_iter"], kv["max_tick_gap_ms"], kv["cpus"], fmtCPUs(a.Pin), proc, extra, e.fakeTag)
			rows = append(rows, labArmRow{Arm: a.Name, Rep: rep, GapMs: gap, Verdict: verdict})
		}
	}
	for _, g := range labArmSummary(rows) {
		t.Logf("LAB_ARMTABLE arm=%s runs=%d stalled=%d median_gap_ms=%.1f max_gap_ms=%.1f median_stalled_gap_ms=%.1f version=%s%s", g.Arm, g.Runs, g.Stalled, g.MedianMs, g.MaxMs, g.MedStallMs, ver, e.fakeTag)
	}
	t.Logf("LAB_DONE lab=GO runs=%d elapsed_min=%.1f%s", len(rows), time.Since(start).Minutes(), e.fakeTag)
}
