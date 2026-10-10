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
//	CELERIS_PROBE_PATCH          comma list of registered runtime patches (t8_patches_linux_test.go: noop, backoff,
//	                             min100us, acklatency, negctl-buildfail); "none" = an unpatched arm in the same run.
//	                             Empty = no patch machinery at all. Each arm is built once per toolchain with
//	                             `go build -overlay` and the arms run interleaved rep by rep. When a patch is set and
//	                             CELERIS_PROBE_UP_CASES is not, the cases default to "little" only.
//
// Budget per version and arch: arms x cases x REPS x (SECS + ~1) s plus the first download and build.
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
// A name that is set but not registered makes that arm verdict NOPATCH and nothing is run for it: an
// unpatched run must never be mistaken for a patched one. Every row carries patch=<name> ("none" for the
// unpatched arm of a patch run, "" when CELERIS_PROBE_PATCH is empty). After each patched build the
// harness logs MATRIX_FACTS ... suspendG_size (go tool nm -size), suspendG_insns_sha (go tool objdump) and the sha256 of the overlaid preempt.go.
var upstreamPatches = map[string]func(goroot, scratch string) (overlayJSON string, err error){}

// ---- helpers --------------------------------------------------------------------------------------

type upEnv struct{ tmp, toolchain, modcache string } // modcache "" = tmp/gomodcache

func (e upEnv) modCache() string {
	if e.modcache != "" {
		return e.modcache
	}
	return filepath.Join(e.tmp, "gomodcache")
}

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
		"GOMODCACHE="+e.modCache(), "HOME="+filepath.Join(e.tmp, "home"),
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
	patch                           string // arm name as printed in rows: "", "none" or a registered patch
	env                             upEnv
}

var nmSizeRe = regexp.MustCompile(`(?m)^\s*[0-9a-f]+\s+(\d+)\s+T\s+runtime\.suspendG$`)

func TestUpstreamMatrix(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("MATRIX_SKIP no go command on PATH: %v", err)
	}
	versions := envList("CELERIS_PROBE_GOVERSIONS", "local")
	reps, secs := envInt("CELERIS_PROBE_REPS", 6), envInt("CELERIS_PROBE_UP_SECS", 30)
	thr := envInt("CELERIS_PROBE_UP_THRESHOLD_MS", 50)
	patchList := envList("CELERIS_PROBE_PATCH", "")
	arms := patchList
	if len(arms) == 0 {
		arms = []string{""}
	}
	patch := strings.Join(patchList, ",")
	defCases := "little,big,little-noasync"
	if len(patchList) > 0 {
		defCases = "little"
	}
	var cases []string
	for _, c := range envList("CELERIS_PROBE_UP_CASES", defCases) {
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
	t.Logf("MATRIX_FACTS estimate: %d versions x %d arms x %d cases x %d reps x ~%d s = ~%d min of runs, plus downloads and builds",
		len(versions), len(arms), len(cases), reps, secs+1, (len(versions)*len(arms)*len(cases)*reps*(secs+1)+59)/60)

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
		cancel()
		for _, arm := range arms {
			bins = append(bins, upBuildArm(t, goBin, srcDir, tmp, e, v, arm, goroot, strings.TrimSpace(goVer), mod[1])...)
		}
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
					kv["ns_per_iter"], kv["work_iters"], kv["go"], kv["goarch"], kv["class"], kv["hetero"], kv["cpus"], kv["gomaxprocs"], kv["max_tick_gap_ms"], b.defGODEBUG, b.patch)
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

// upBuildArm builds the reproducer for one toolchain and one patch arm ("" = no patch machinery,
// "none" = unpatched arm of a patch run, else a registered patch applied with go build -overlay).
// It returns zero or one upBin; every failure is a MATRIX_SKIP row, never a test failure.
func upBuildArm(t *testing.T, goBin, srcDir, tmp string, e upEnv, v, arm, goroot, goVer, goMod string) []upBin {
	name := "stallrepro-" + v
	if arm != "" {
		name += "-" + arm
	}
	out := filepath.Join(tmp, name)
	args := []string{"build", "-o", out}
	if arm != "" {
		// A toolchain that GOTOOLCHAIN=go<ver> downloaded lives in GOMODCACHE, and the go command of go1.27.2 refuses an
		// overlay that replaces "files beneath GOMODCACHE" (found by a 1.27.2 smoke run: every patched arm failed with
		// that message; 1.20.14 and 1.24.6 did not check). So every arm of a patch run uses that toolchain's own go binary
		// directly (GOTOOLCHAIN=local, GOROOT inferred from the binary) with a GOMODCACHE of its own elsewhere.
		goBin = filepath.Join(goroot, "bin", "go")
		e.toolchain, e.modcache = "local", filepath.Join(tmp, "gomodcache-"+v+"-"+arm)
		_ = os.MkdirAll(e.modcache, 0o755)
	}
	if arm != "" && arm != "none" {
		fn := upstreamPatches[arm]
		if fn == nil {
			t.Logf("MATRIX_SKIP version=%s patch=%q verdict=NOPATCH reason=%q", v, arm, "CELERIS_PROBE_PATCH has "+arm+", which is not registered in upstreamPatches")
			return nil
		}
		scratch := filepath.Join(tmp, "patch-"+v+"-"+arm)
		_ = os.MkdirAll(scratch, 0o755)
		ov, err := fn(goroot, scratch)
		if err != nil {
			t.Logf("MATRIX_SKIP version=%s patch=%q reason=%q", v, arm, "patch failed: "+err.Error())
			return nil
		}
		args = append(args, "-overlay="+ov)
		if b, err := os.ReadFile(filepath.Join(scratch, "preempt.go")); err == nil {
			o, _ := os.ReadFile(filepath.Join(goroot, "src/runtime/preempt.go"))
			t.Logf("MATRIX_FACTS version=%s patch=%q overlaid preempt.go sha256 %x (toolchain's own %x), %d vs %d bytes", v, arm, sha256.Sum256(b), sha256.Sum256(o), len(b), len(o))
		}
	}
	bctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute) // a patched runtime is rebuilt from source in an empty GOCACHE
	_, se, err := upRun(bctx, srcDir, e.env(), goBin, append(args, ".")...)
	cancel()
	if err != nil {
		t.Logf("MATRIX_SKIP version=%s patch=%q reason=%q", v, arm, "build failed: "+err.Error()+" "+tail(se, 600))
		return nil
	}
	mctx, mcancel := context.WithTimeout(context.Background(), time.Minute)
	defer mcancel()
	// the GODEBUG defaults the go.mod go line gave this binary (go1.21+ records them as a build setting)
	mi, _, _ := upRun(mctx, srcDir, e.env(), goBin, "version", "-m", out)
	dg := ""
	if m := regexp.MustCompile(`DefaultGODEBUG=(\S+)`).FindStringSubmatch(mi); m != nil {
		dg = m[1]
	}
	// proof that the runtime in this binary is the one compiled from the overlay: the size of runtime.suspendG
	size := "unknown"
	if nm, _, err := upRun(mctx, srcDir, e.env(), goBin, "tool", "nm", "-size", out); err == nil {
		if m := nmSizeRe.FindStringSubmatch(nm); m != nil {
			size = m[1]
		}
	}
	// and a hash of its instruction encodings (no addresses): equal for the unpatched and the noop arm,
	// different for any arm that changes suspendG (a changed constant keeps the size but not the hash)
	dis := "unknown"
	if od, _, err := upRun(mctx, srcDir, e.env(), goBin, "tool", "objdump", "-s", `^runtime\.suspendG$`, out); err == nil {
		var norm []string // objdump columns are separated by runs of tabs: file:line, address, encoding, instruction
		for _, l := range strings.Split(od, "\n") {
			var f []string
			for _, c := range strings.Split(l, "\t") {
				if c = strings.TrimSpace(c); c != "" {
					f = append(f, c)
				}
			}
			if len(f) >= 4 {
				norm = append(norm, f[2]) // the machine encoding: address-free, relative branches included
			}
		}
		dis = fmt.Sprintf("%d-insns-%x", len(norm), sha256.Sum256([]byte(strings.Join(norm, "\n"))))[:32]
	}
	t.Logf("MATRIX_FACTS version=%s patch=%q built with %s (go.mod go %s) GOROOT=%s DefaultGODEBUG=%q suspendG_size=%s suspendG_insns_sha=%s", v, arm, goVer, goMod, goroot, dg, size, dis)
	return []upBin{{label: v, path: out, goroot: goroot, env: e, defGODEBUG: dg, patch: arm}}
}
