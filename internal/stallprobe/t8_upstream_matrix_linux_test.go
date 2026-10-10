//go:build linux

package stallprobe

// T8: the upstream-Go matrix. Data collection for a golang/go report, not a pass/fail test: it
// ALWAYS exits 0 (a rep that stalls, hangs or cannot be built is a table row, not a failure).
//
//	go test -run '^TestUpstreamMatrix$' -v     (celeris-stress: -f run='^TestUpstreamMatrix$')
//
// It writes the standalone reproducer (testdata/upstream/stallrepro.go: package main, standard
// library only, see its doc comment) and a C signal-cost program (sigcost.c) to a temp dir. For
// each toolchain it builds the reproducer once (GOTOOLCHAIN=go<ver>, a go.mod whose go line is the
// toolchain's own major.minor, so the default GODEBUG settings of that Go release apply, never a
// blanket old line) and then runs, rep by rep with the cases interleaved:
//
//	little        -class=little (the A520s on msr1; on a one-core-type host the 4 lowest CPU ids)
//	big           -class=big (the A720s; the 8 highest ids): no stall expected
//	little-noasync  little with GODEBUG=asyncpreemptoff=1: no stall expected
//
// The C program runs once per dispatch per CPU class. Everything is logged through t.Log with the
// prefixes MATRIX_* and SIGCOST_*; a TSV row is
//
//	MATRIX<TAB>version<TAB>case<TAB>rep<TAB>max_gap_ms<TAB>gc_cycles<TAB>verdict<TAB>k=v ...
//
// verdict OK | STALL (max wake gap above the threshold) | HANG (the run did not end in secs+30 s,
// counted as a stall) | ERROR (no RESULT line). The child's stdout never reaches the test's stdout.
// Nothing is written outside the temp dir (HOME, XDG_*, GOCACHE, GOMODCACHE, GOPATH are all in it).
//
// Knobs (celeris-stress extra=NAME=V, values [A-Za-z0-9_.,:/+-]):
//
//	CELERIS_PROBE_GOVERSIONS     comma list: 1.22.12,1.24.6,1.27.2 -> GOTOOLCHAIN=go<ver>; "local" = the go on
//	                             PATH (GOTOOLCHAIN=local, no download); default local
//	CELERIS_PROBE_REPS           reps per (version, case) (6). NOTE: the P2/P3 probes read the same name
//	                             with a default of 100; under run=^TestUpstreamMatrix$ they do not run.
//	CELERIS_PROBE_UP_SECS        seconds per run (30)
//	CELERIS_PROBE_UP_THRESHOLD_MS  STALL above this longest wake gap (50)
//	CELERIS_PROBE_UP_CASES       subset of little,big,little-noasync (all three)
//	CELERIS_PROBE_PATCH          name of a registered runtime patch (see upstreamPatches); empty = none
//
// Budget per version and arch: 3 cases x REPS x (SECS + ~1) s plus the first download and build.
// Default 6 reps x 30 s: about 9.5 min per version per arch; the dispatch timeout must cover
// versions x that, plus about 3 min for the C program and the downloads.

import (
	"bytes"
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

//go:embed testdata/upstream/stallrepro.go
var upReproSrc []byte

//go:embed testdata/upstream/sigcost.c
var upSigcostSrc []byte

// ---- PATCH HOOK (consumed by the 'patch' agent) ------------------------------------------------
//
// CELERIS_PROBE_PATCH=<name> looks <name> up here. A patch is a function that is given the GOROOT
// of the toolchain under test (read-only: a downloaded toolchain is read-only) and a scratch dir,
// writes its modified copies of GOROOT files into the scratch dir, and returns the path of a
// `go build -overlay` JSON file that maps the GOROOT originals to those copies ({"Replace":
// {"<goroot>/src/runtime/preempt.go": "<scratch>/preempt.go"}}). Register it from an init() in a
// file of its own (e.g. t8_patches_linux_test.go) with
//
//	func init() { upstreamPatches["suspendg-backoff"] = func(goroot, scratch string) (string, error) {...} }
//
// A name that is set but not registered makes every row verdict NOPATCH and nothing is run: an
// unpatched run must never be mistaken for a patched one. Every row carries patch=<name>.
var upstreamPatches = map[string]func(goroot, scratch string) (overlayJSON string, err error){}

// ---- helpers --------------------------------------------------------------------------------------

type upEnv struct{ tmp, toolchain string }

// env is the child environment: nothing inherited that could redirect the toolchain or write outside tmp.
func (e upEnv) env(extra ...string) []string {
	var out []string
	drop := regexp.MustCompile(`^(GOROOT|GOTOOLCHAIN|GOFLAGS|GOWORK|GODEBUG|GOGC|GOMAXPROCS|GOENV|GOPATH|GOCACHE|GOMODCACHE|HOME|XDG_[A-Z_]+|TMPDIR|CELERIS_[A-Z0-9_]*|GO111MODULE|GOEXPERIMENT|GOARCH|GOOS|CGO_ENABLED)=`)
	for _, kv := range os.Environ() {
		if !drop.MatchString(kv) {
			out = append(out, kv)
		}
	}
	return append(out, "GOTOOLCHAIN="+e.toolchain, "GOFLAGS=-modcacherw", "GOENV=off", "GOWORK=off",
		"GOPATH="+filepath.Join(e.tmp, "gopath"), "GOCACHE="+filepath.Join(e.tmp, "gocache"),
		"GOMODCACHE="+filepath.Join(e.tmp, "gomodcache"), "HOME="+filepath.Join(e.tmp, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(e.tmp, "xdg"), "TMPDIR="+e.tmp, "GO111MODULE=on", "CGO_ENABLED=0",
		"GOGC=100") // GOMAXPROCS stays unset: the runtime default
}

func upRun(ctx context.Context, dir string, env []string, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return strings.ReplaceAll(s, "\n", " | ")
}

var kvRe = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)

func parseKV(line string) map[string]string {
	m := map[string]string{}
	for _, g := range kvRe.FindAllStringSubmatch(line, -1) {
		m[g[1]] = strings.Trim(g[2], `"`)
	}
	return m
}

type upBin struct {
	label, path, goroot, defGODEBUG string
	env                             upEnv
}

func TestUpstreamMatrix(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("MATRIX_SKIP no go command on PATH: %v", err)
	}
	versions := envList("CELERIS_PROBE_GOVERSIONS", "local")
	reps, secs := envInt("CELERIS_PROBE_REPS", 6), envInt("CELERIS_PROBE_UP_SECS", 30)
	thr := envInt("CELERIS_PROBE_UP_THRESHOLD_MS", 50)
	patch := os.Getenv("CELERIS_PROBE_PATCH")
	var cases []string
	for _, c := range envList("CELERIS_PROBE_UP_CASES", "little,big,little-noasync") {
		if c == "little" || c == "big" || c == "little-noasync" {
			cases = append(cases, c)
		}
	}
	tmp, err := os.MkdirTemp("", "upmatrix-")
	if err != nil {
		t.Skipf("MATRIX_SKIP no temp dir: %v", err)
	}
	defer func() { // the module cache and downloaded toolchains are read-only: make them writable first
		_ = filepath.Walk(tmp, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
		if err := os.RemoveAll(tmp); err != nil {
			t.Logf("MATRIX_NOTE cleanup of %s: %v", tmp, err)
		}
	}()
	for _, d := range []string{"gopath", "gocache", "gomodcache", "home", "xdg"} {
		_ = os.MkdirAll(filepath.Join(tmp, d), 0o755)
	}
	sum := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b))[:16] }
	t.Logf("MATRIX_FACTS %s | go on PATH: %s | versions %v reps %d secs %d threshold_ms %d cases %v patch %q | stallrepro.go sha256 %s sigcost.c sha256 %s",
		hostFacts(), goBin, versions, reps, secs, thr, cases, patch, sum(upReproSrc), sum(upSigcostSrc))
	tp := loadTopo()
	t.Logf("MATRIX_FACTS topology: %s", tp.describe())
	t.Logf("MATRIX_FACTS estimate: %d versions x %d cases x %d reps x ~%d s = ~%d min of runs, plus downloads and builds",
		len(versions), len(cases), reps, secs+1, (len(versions)*len(cases)*reps*(secs+1)+59)/60)

	srcDir := filepath.Join(tmp, "src")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "main.go"), upReproSrc, 0o644)
	_ = os.WriteFile(filepath.Join(srcDir, "sigcost.c"), upSigcostSrc, 0o644)

	// 1. the C program, once per CPU class
	upSigcost(t, srcDir, tmp, tp)

	// 2. one binary per toolchain
	var bins []upBin
	for _, v := range versions {
		tc := "go" + strings.TrimPrefix(v, "go")
		if v == "local" {
			tc = "local"
		}
		e := upEnv{tmp: tmp, toolchain: tc}
		bctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		goVer, se, err := upRun(bctx, srcDir, e.env(), goBin, "version") // downloads the toolchain when needed (go env GOVERSION needs go1.17+)
		if err != nil {
			cancel()
			t.Logf("MATRIX_SKIP version=%s reason=%q", v, "toolchain unavailable: "+err.Error()+" "+tail(se, 300))
			continue
		}
		mod := regexp.MustCompile(`go version go(\d+\.\d+)`).FindStringSubmatch(goVer)
		if mod == nil {
			cancel()
			t.Logf("MATRIX_SKIP version=%s reason=%q", v, "cannot parse GOVERSION "+goVer)
			continue
		}
		_ = os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module stallrepro\n\ngo "+mod[1]+"\n"), 0o644)
		goroot, _, _ := upRun(bctx, srcDir, e.env(), goBin, "env", "GOROOT")
		goroot = strings.TrimSpace(goroot)
		args := []string{"build", "-o", filepath.Join(tmp, "stallrepro-"+v)}
		if patch != "" {
			fn := upstreamPatches[patch]
			if fn == nil {
				cancel()
				t.Logf("MATRIX_SKIP version=%s verdict=NOPATCH reason=%q", v, "CELERIS_PROBE_PATCH="+patch+" is not registered in upstreamPatches")
				continue
			}
			scratch := filepath.Join(tmp, "patch-"+v)
			_ = os.MkdirAll(scratch, 0o755)
			ov, err := fn(goroot, scratch)
			if err != nil {
				cancel()
				t.Logf("MATRIX_SKIP version=%s reason=%q", v, "patch "+patch+": "+err.Error())
				continue
			}
			args = append(args, "-overlay="+ov)
		}
		_, se, err = upRun(bctx, srcDir, e.env(), goBin, append(args, ".")...)
		cancel()
		if err != nil {
			t.Logf("MATRIX_SKIP version=%s reason=%q", v, "build failed: "+err.Error()+" "+tail(se, 600))
			continue
		}
		// the GODEBUG defaults the go.mod go line gave this binary (go1.21+ records them as a build setting)
		mctx, mcancel := context.WithTimeout(context.Background(), time.Minute)
		mi, _, _ := upRun(mctx, srcDir, e.env(), goBin, "version", "-m", args[2])
		mcancel()
		defGD := regexp.MustCompile(`DefaultGODEBUG=(\S+)`).FindStringSubmatch(mi)
		dg := ""
		if defGD != nil {
			dg = defGD[1]
		}
		t.Logf("MATRIX_FACTS version=%s built with %s (go.mod go %s) GOROOT=%s DefaultGODEBUG=%q", v, strings.TrimSpace(goVer), mod[1], goroot, dg)
		bins = append(bins, upBin{label: v, path: args[2], goroot: goroot, env: e, defGODEBUG: dg})
	}

	// 3. rep outer, then version, then case: drift in the host never lines up with one case
	t.Logf("MATRIX\tversion\tcase\trep\tmax_gap_ms\tgc_cycles\tverdict\textra")
	for rep := 1; rep <= reps; rep++ {
		for _, b := range bins {
			for _, c := range cases {
				flags := []string{"-secs=" + strconv.Itoa(secs), "-threshold-ms=" + strconv.Itoa(thr), "-class=little"}
				env := b.env.env()
				switch c {
				case "big":
					flags[2] = "-class=big"
				case "little-noasync":
					env = append(env, "GODEBUG=asyncpreemptoff=1")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs+30)*time.Second)
				out, se, err := upRun(ctx, tmp, env, b.path, flags...)
				hung := ctx.Err() != nil
				cancel()
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
				extra := fmt.Sprintf("ns_per_iter=%s work_iters=%s go=%s goarch=%s class=%s hetero=%s cpus=%s gomaxprocs=%s tick_gap_ms=%s default_godebug=%q patch=%q",
					kv["ns_per_iter"], kv["work_iters"], kv["go"], kv["goarch"], kv["class"], kv["hetero"], kv["cpus"], kv["gomaxprocs"], kv["max_tick_gap_ms"], b.defGODEBUG, patch)
				if verdict == "HANG" || verdict == "ERROR" {
					extra += fmt.Sprintf(" err=%v stderr=%q", err, tail(se, 400))
				}
				t.Logf("MATRIX\t%s\t%s\t%d\t%s\t%s\t%s\t%s", b.label, c, rep, kv["max_gap_ms"], kv["gc_cycles"], verdict, extra)
			}
		}
	}
	t.Logf("MATRIX_DONE versions_built=%d of %d", len(bins), len(versions))
}

// upSigcost builds sigcost.c and runs it once per CPU class: the target on the class's first CPU,
// the sender on its second (same class) and, for the slowest class, also on the fastest class.
func upSigcost(t *testing.T, srcDir, tmp string, tp topo) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		if cc, err = exec.LookPath("gcc"); err != nil {
			t.Logf("SIGCOST_SKIP no C compiler (cc, gcc) on PATH")
			return
		}
	}
	exe := filepath.Join(tmp, "sigcost")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, se, err := upRun(ctx, srcDir, os.Environ(), cc, "-O2", "-pthread", "-o", exe, "sigcost.c"); err != nil {
		t.Logf("SIGCOST_SKIP compile failed: %v %s", err, tail(se, 500))
		return
	}
	type run struct {
		label          string
		target, sender int
	}
	var runs []run
	for i, cl := range tp.Classes {
		name := "class" + strconv.Itoa(i) + "-" + classLabel(tp, cl[0])
		if len(cl) > 1 {
			runs = append(runs, run{name + "-sender-same-class", cl[0], cl[1]})
		}
		if i == len(tp.Classes)-1 && len(tp.Classes) > 1 {
			runs = append(runs, run{name + "-sender-fastest-class", cl[0], tp.Classes[0][0]})
		}
	}
	if len(runs) == 0 && len(tp.Allowed) > 1 {
		runs = append(runs, run{"all-sender-other", tp.Allowed[0], tp.Allowed[1]})
	}
	for _, r := range runs {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, se, err := upRun(rctx, tmp, os.Environ(), exe, strconv.Itoa(r.target), strconv.Itoa(r.sender), "100000")
		rcancel()
		if err != nil {
			t.Logf("SIGCOST_SKIP %s target=%d sender=%d: %v %s", r.label, r.target, r.sender, err, tail(se, 300))
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			t.Logf("SIGCOST_MATRIX %s sender_cpu=%d %s", r.label, r.sender, l)
		}
	}
}
