//go:build linux

package stallprobe

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// P2 StdlibLarge. A pure net/http server (no celeris in the process), in a
// fresh process, serves 4 MiB and 64 MiB bodies to a loopback client in the
// same process with the same rule as celeris's large-response tests: no byte
// for 5 s fails the repetition. 20 repetitions per size by default. The
// question: does plain Go stall on this host? A host that stalls net/http the
// same way as celeris is not celeris's problem. The process also runs a 1 ms
// watchdog, so a stall of the whole process (not of one connection) shows.

type p2Result struct {
	Pid       int
	GoMaxProc int
	NumCPU    int
	Allowed   string
	Leaves    []leafResult
	WD        wdResult
}

func p2Child() (any, error) {
	c := newClk()
	sizes, err := sizesMiB()
	if err != nil {
		return nil, err
	}
	reps := envInt("CELERIS_PROBE_REPS", 20)
	maxStalls := envInt("CELERIS_PROBE_MAXSTALLS", 3)
	idle := time.Duration(envInt("CELERIS_PROBE_IDLE_MS", 5000)) * time.Millisecond
	bodies := map[int][]byte{}
	for _, m := range sizes {
		bodies[m] = patterned(m << 20)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		m, _ := strconv.Atoi(r.URL.Query().Get("mib"))
		b, ok := bodies[m]
		if !ok {
			http.Error(w, "no such body", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	addr := ln.Addr().String()

	allowed := loadTopo()
	res := &p2Result{Pid: os.Getpid(), GoMaxProc: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), Allowed: fmtCPUs(allowed.Allowed)}
	wd := startWatchdog(c)
	buf := make([]byte, 64<<20)
	for _, m := range sizes {
		if len(buf) < m<<20 {
			buf = make([]byte, m<<20)
		}
		l := runLeaf(c, fmt.Sprintf("%dMiB", m), m, addr, "/big?mib="+strconv.Itoa(m), bodies[m], buf, reps, maxStalls, idle)
		res.Leaves = append(res.Leaves, l)
	}
	res.WD = wd.Stop()
	return res, nil
}

func sizesMiB() ([]int, error) {
	var out []int
	for _, s := range envList("CELERIS_PROBE_SIZES", "4,64") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 512 {
			return nil, fmt.Errorf("CELERIS_PROBE_SIZES: %q is not a size in MiB", s)
		}
		out = append(out, n)
	}
	return out, nil
}

func TestProbeP2StdlibLarge(t *testing.T) {
	tp := loadTopo()
	reps := envInt("CELERIS_PROBE_REPS", 20)
	idleMs := envInt("CELERIS_PROBE_IDLE_MS", 5000)
	sizes, err := sizesMiB()
	if err != nil {
		t.Fatal(err)
	}
	budget := time.Duration(len(sizes)*(reps+envInt("CELERIS_PROBE_MAXSTALLS", 3)*(idleMs/1000+2))+120) * time.Second
	so, se, err := runChild(childSpec{kind: "p2", timeout: budget})
	if err != nil {
		t.Fatalf("the child failed: %v\n%s", err, tailLines(se, 20))
	}
	var res p2Result
	if err := decodeChild(so, &res); err != nil {
		t.Fatalf("%v\n%s", err, tailLines(se, 20))
	}
	logf(t, "net/http only, fresh process pid %d, GOMAXPROCS %d, NumCPU %d, allowed CPUs %s\nwatchdog over the whole child: max oversleep %s ms (%d sleeps)", res.Pid, res.GoMaxProc, res.NumCPU, res.Allowed, ms(res.WD.MaxNs), res.WD.Sleeps)
	for _, l := range res.Leaves {
		t.Run(l.Name, func(t *testing.T) { reportLeaf(t, tp, l, res.WD) })
	}
}
