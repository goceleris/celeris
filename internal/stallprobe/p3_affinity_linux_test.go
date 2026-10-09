//go:build linux

package stallprobe

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
)

// P3 AffinityA720. The large-response check (P2's client, celeris's 5 s
// no-byte rule) against a celeris server whose process is restricted to a set
// of CPUs, to test the big.LITTLE hypothesis: the stalls need loops or
// handlers on the slow cores.
//
// How the engine picks its CPUs (internal/engine/epoll/engine.go,
// internal/platform/cpu_linux.go): Workers defaults to GOMAXPROCS, and loop i is
// pinned to CPU number (i % runtime.NumCPU()), a CPU INDEX 0..n-1, not a
// member of the process's mask. So a process started on cpus {0,1,6-11}
// still pins its loops to 0..7, which on msr1 includes A520s (cpus 2-5).
// Restricting the process alone is therefore not honest. Each case here
//
//  1. starts the engine in a fresh child process whose mask is the case's CPU
//     set from its first instruction (the parent sets its thread's mask just
//     before the fork), so NumCPU and GOMAXPROCS are the set's size and every
//     Go thread, the client's included, is confined to the set;
//  2. sets Config.Workers to the set's size;
//  3. after Start returns (every loop has run its own PinToCPU by then:
//     the epoll Listen waits for each loop's ready signal, which is sent after
//     the pin) finds the loop threads (the threads that are pinned to exactly
//     one CPU and were not before Start) and moves loop k, in the order the
//     engine pinned them, onto the k-th CPU of the set with sched_setaffinity
//     on its tid;
//  4. prints the thread table before the workload and again after it, so a
//     reader sees where every loop ran.
//
// Cases (same names on every host; see topo.caseCPUs): all (nothing
// restricted, the default the failing run had), fast8 (the 8 fastest-core-type
// CPUs: msr1's A720s), fast4 (the 4 fastest of those), slow4 (the slowest core
// type: msr1's 4 A520s). fast4 next to slow4 separates "slow cores" from "only
// 4 CPUs". std is the control: the same process shape with no engine loops.

type p3Leaf struct {
	Shape string
	leafResult
}

type p3Result struct {
	Pid       int
	Engine    string
	Case      string
	GoMaxProc int
	NumCPU    int
	Allowed   string
	Workers   int
	Leaves    []p3Leaf
	WD        wdResult
	Notes     []string
}

func engineType(name string) (celeris.EngineType, error) {
	switch name {
	case "epoll":
		return celeris.Epoll, nil
	case "std":
		return celeris.Std, nil
	case "io_uring":
		return celeris.IOUring, nil
	case "adaptive":
		return celeris.Adaptive, nil
	}
	return 0, fmt.Errorf("unknown engine %q (epoll, std, io_uring, adaptive)", name)
}

func p3Child() (any, error) {
	c := newClk()
	engName := os.Getenv("CELERIS_PROBE_ARG_ENGINE")
	et, err := engineType(engName)
	if err != nil {
		return nil, err
	}
	target, err := parseCPUList(os.Getenv("CELERIS_PROBE_ARG_TARGET"))
	if err != nil {
		return nil, err
	}
	workers := envInt("CELERIS_PROBE_ARG_WORKERS", 0)
	sizes, err := sizesMiB()
	if err != nil {
		return nil, err
	}
	reps := envInt("CELERIS_PROBE_REPS", 20)
	maxStalls := envInt("CELERIS_PROBE_MAXSTALLS", 3)
	idle := time.Duration(envInt("CELERIS_PROBE_IDLE_MS", 5000)) * time.Millisecond
	tp := loadTopo()
	res := &p3Result{Pid: os.Getpid(), Engine: engName, Case: os.Getenv("CELERIS_PROBE_ARG_CASE"), GoMaxProc: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), Allowed: fmtCPUs(tp.Allowed), Workers: workers}
	bodies := map[int][]byte{}
	maxMiB := 0
	for _, m := range sizes {
		bodies[m] = patterned(m << 20)
		maxMiB = max(maxMiB, m)
	}
	buf := make([]byte, maxMiB<<20)
	wd := startWatchdog(c)

	for _, shape := range envList("CELERIS_PROBE_SHAPES", "sync,async-route") {
		asyncServer, asyncRoute := false, false
		switch shape {
		case "sync":
		case "async-loop":
			asyncServer = true
		case "async-route":
			asyncRoute = true
		default:
			return nil, fmt.Errorf("unknown shape %q (sync, async-loop, async-route)", shape)
		}
		srv, addr, loops, notes, err := p3Start(et, workers, asyncServer, asyncRoute, bodies, target, engName)
		res.Notes = append(res.Notes, fmt.Sprintf("[%s] %s", shape, strings.Join(notes, "\n    ")))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shape, err)
		}
		for _, m := range sizes {
			l := runLeaf(c, fmt.Sprintf("%dMiB", m), m, addr, "/big?n="+strconv.Itoa(m), bodies[m], buf, reps, maxStalls, idle)
			res.Leaves = append(res.Leaves, p3Leaf{Shape: shape, leafResult: l})
		}
		res.Notes = append(res.Notes, fmt.Sprintf("[%s] loop threads after the workload: %s", shape, loopTable(loops)))
		srv.stop()
	}
	res.WD = wd.Stop()
	return res, nil
}

type p3Server struct {
	s         *celeris.Server
	startDone chan error
}

func (p *p3Server) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.s.Shutdown(ctx)
	select {
	case <-p.startDone:
	case <-time.After(15 * time.Second):
	}
}

type loopThread struct {
	Tid  int
	From int // the CPU the engine pinned it to
	To   int // the CPU it was moved to, -1 when not moved
}

func loopTable(ls []loopThread) string {
	if len(ls) == 0 {
		return "none found (no pinned loop threads)"
	}
	var parts []string
	for _, l := range ls {
		parts = append(parts, fmt.Sprintf("tid %d engine-pinned cpu %d, now allowed %s", l.Tid, l.From, threadAllowed(l.Tid)))
	}
	return strings.Join(parts, "; ")
}

// p3Start starts one celeris server and, for a case with a CPU set, moves its
// loop threads into the set.
func p3Start(et celeris.EngineType, workers int, asyncServer, asyncRoute bool, bodies map[int][]byte, target []int, engName string) (*p3Server, string, []loopThread, []string, error) {
	var notes []string
	before := pinnedThreads()
	var addr string
	var srv *p3Server
	cfg := celeris.Config{Engine: et, AsyncHandlers: asyncServer, Workers: workers, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", nil, notes, err
		}
		addr = ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s := celeris.New(cfg)
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		big := s.GET("/big", func(c *celeris.Context) error {
			n, err := strconv.Atoi(c.Query("n"))
			if err != nil {
				return err
			}
			body, ok := bodies[n]
			if !ok {
				return fmt.Errorf("no body of %d MiB", n)
			}
			return c.Blob(http.StatusOK, "application/octet-stream", body)
		})
		if asyncRoute {
			big.Async()
		}
		srv = &p3Server{s: s, startDone: make(chan error, 1)}
		go func() { srv.startDone <- s.Start() }()
		err = waitPing(addr, srv.startDone)
		if err == nil {
			break
		}
		srv.stop()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return nil, "", nil, notes, fmt.Errorf("server did not start: %w", err)
	}

	// The loop threads: pinned to exactly one CPU now, not before Start.
	want := workers
	if want == 0 {
		want = runtime.GOMAXPROCS(0)
	}
	var loops []loopThread
	if engName == "epoll" {
		deadline := time.Now().Add(3 * time.Second)
		for {
			loops = loops[:0]
			for tid, cpu := range pinnedThreads() {
				if _, was := before[tid]; !was {
					loops = append(loops, loopThread{Tid: tid, From: cpu, To: -1})
				}
			}
			if len(loops) >= want || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		sort.Slice(loops, func(i, j int) bool {
			if loops[i].From != loops[j].From {
				return loops[i].From < loops[j].From
			}
			return loops[i].Tid < loops[j].Tid
		})
		notes = append(notes, fmt.Sprintf("epoll loops expected %d, found %d pinned loop threads", want, len(loops)))
	} else {
		notes = append(notes, fmt.Sprintf("engine %s: loop threads are not located (only epoll's are), mask applies to the whole process", engName))
	}
	notes = append(notes, "loop threads as the engine pinned them: "+loopTable(loops))
	if len(target) > 0 && len(loops) > 0 {
		for i := range loops {
			cpu := target[i%len(target)]
			m := maskOf([]int{cpu})
			if err := unix.SchedSetaffinity(loops[i].Tid, &m); err != nil {
				notes = append(notes, fmt.Sprintf("moving tid %d to cpu %d failed: %v", loops[i].Tid, cpu, err))
				continue
			}
			loops[i].To = cpu
		}
		var mv []string
		for _, l := range loops {
			mv = append(mv, fmt.Sprintf("tid %d cpu %d -> %d", l.Tid, l.From, l.To))
		}
		notes = append(notes, "loop threads moved into the case's set: "+strings.Join(mv, "; "))
		// The ping was served before the move; make sure the server answers after it.
		if err := waitPing(addr, nil); err != nil {
			return srv, addr, loops, notes, fmt.Errorf("no answer after the move: %w", err)
		}
	}
	return srv, addr, loops, notes, nil
}

func waitPing(addr string, startDone <-chan error) error {
	probe := &http.Client{Timeout: 300 * time.Millisecond}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if startDone != nil {
			select {
			case err := <-startDone:
				if err == nil {
					err = fmt.Errorf("Start returned nil before the server was ready")
				}
				return err
			default:
			}
		}
		if resp, err := probe.Get("http://" + addr + "/ping"); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no answer on /ping at %s within 15s", addr)
}

func TestProbeP3Affinity(t *testing.T) {
	tp := loadTopo()
	logf(t, "host topology:\n%s", tp.describe())
	cases := envList("CELERIS_PROBE_CASES", "all,fast8,fast4,slow4")
	engines := envList("CELERIS_PROBE_ENGINES", "epoll,std")
	sizes, err := sizesMiB()
	if err != nil {
		t.Fatal(err)
	}
	reps := envInt("CELERIS_PROBE_REPS", 20)
	idleMs := envInt("CELERIS_PROBE_IDLE_MS", 5000)
	nShapes := len(envList("CELERIS_PROBE_SHAPES", "sync,async-route"))
	budget := time.Duration(nShapes*len(sizes)*(reps+envInt("CELERIS_PROBE_MAXSTALLS", 3)*(idleMs/1000+2))+180) * time.Second
	for _, cs := range cases {
		cpus, note, err := tp.caseCPUs(cs)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		t.Run(cs, func(t *testing.T) {
			var cl []string
			for _, c := range cpus {
				cl = append(cl, fmt.Sprintf("%d(%s,cap %d)", c, tp.CPU[c].class(), tp.CPU[c].Cap))
			}
			logf(t, "case %s: %d CPUs, %s: %s", cs, len(cpus), note, strings.Join(cl, " "))
			for _, eng := range engines {
				t.Run(eng, func(t *testing.T) {
					env := []string{
						"CELERIS_PROBE_ARG_CASE=" + cs,
						"CELERIS_PROBE_ARG_ENGINE=" + eng,
					}
					var mask []int
					if cs != "all" {
						mask = cpus
						env = append(env, "CELERIS_PROBE_ARG_TARGET="+fmtCPUs(cpus), "CELERIS_PROBE_ARG_WORKERS="+strconv.Itoa(len(cpus)))
					}
					so, se, err := runChild(childSpec{kind: "p3", env: env, mask: mask, timeout: budget})
					if err != nil {
						t.Fatalf("the child failed: %v\n%s", err, tailLines(se, 25))
					}
					var res p3Result
					if err := decodeChild(so, &res); err != nil {
						t.Fatalf("%v\n%s", err, tailLines(se, 25))
					}
					logf(t, "child pid %d: GOMAXPROCS %d, NumCPU %d, allowed CPUs %s, Workers %d (0 = GOMAXPROCS)\n%s\nwatchdog over the whole child: max oversleep %s ms (%d sleeps)",
						res.Pid, res.GoMaxProc, res.NumCPU, res.Allowed, res.Workers, strings.Join(res.Notes, "\n"), ms(res.WD.MaxNs), res.WD.Sleeps)
					byShape := map[string][]p3Leaf{}
					var order []string
					for _, l := range res.Leaves {
						if _, ok := byShape[l.Shape]; !ok {
							order = append(order, l.Shape)
						}
						byShape[l.Shape] = append(byShape[l.Shape], l)
					}
					for _, sh := range order {
						t.Run(sh, func(t *testing.T) {
							for _, l := range byShape[sh] {
								t.Run(l.Name, func(t *testing.T) { reportLeaf(t, tp, l.leafResult, res.WD) })
							}
						})
					}
				})
			}
		})
	}
}
