//go:build linux

package stallprobe

// T9: environment facts for the golang/go report. Not a pass/fail test: it ALWAYS exits 0 (a command
// that fails or is not permitted is printed as a FACTS line, never a failure).
//
//	go test -run '^TestUpstreamFacts$' -v     (celeris-stress: -f run='^TestUpstreamFacts$')
//
// Every line it prints starts with "FACTS " (after the testing package's own file:line prefix):
//
//	go version / go env       the go on PATH exactly as this test process sees it (inherited environment), and,
//	                          for each CELERIS_PROBE_GOVERSIONS entry other than "local", the same two commands
//	                          under the matrix child environment (TestUpstreamMatrix's upEnv: GOTOOLCHAIN=go<ver>,
//	                          GOFLAGS=-modcacherw, scratch GOPATH/GOCACHE/GOMODCACHE), which downloads that toolchain
//	uname -a                  kernel and machine
//	cpu <n>                   one compact line per cpu directory under /sys/devices/system/cpu (offline ones too):
//	                          the "CPU implementer/variant/part/revision" fields of /proc/cpuinfo (arm64: the MIDR
//	                          fields), MIDR_EL1 when the root-only sysfs file is readable, cpu_capacity,
//	                          scaling_cur_freq and cpuinfo_max_freq (kHz) as read at that moment
//	dmesg                     `dmesg 2>&1 | grep -i -E "errat|workaround|2966298"`; dmesg is run on its own so a
//	                          permission failure is printed instead of being swallowed by the pipeline
//	lscpu                     when present
//
// Knob: CELERIS_PROBE_GOVERSIONS (same meaning as in TestUpstreamMatrix, default "local").

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// factsRun runs one command and logs its combined output line by line as "FACTS <label>: <line>" and a
// final "FACTS <label>: exit=<status>" line. A failure is only printed.
func factsRun(t *testing.T, label string, timeout time.Duration, dir string, env []string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	out, err := cmd.CombinedOutput()
	factsLines(t, label, string(out))
	if err != nil {
		t.Logf("FACTS %s: FAILED %v", label, err)
	}
}

func factsLines(t *testing.T, label, s string) {
	t.Helper()
	s = strings.TrimRight(s, "\n")
	if s == "" {
		t.Logf("FACTS %s: (no output)", label)
		return
	}
	for _, l := range strings.Split(s, "\n") {
		t.Logf("FACTS %s: %s", label, strings.TrimRight(l, " \t\r"))
	}
}

// factsCPUInfo returns, per processor id, the key/value pairs of its /proc/cpuinfo block.
func factsCPUInfo() map[int]map[string]string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil
	}
	out := map[int]map[string]string{}
	cur := -1
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "processor" {
			n, err := strconv.Atoi(v)
			if err != nil {
				cur = -1
				continue
			}
			cur = n
			out[cur] = map[string]string{}
			continue
		}
		if cur >= 0 {
			if _, dup := out[cur][k]; !dup {
				out[cur][k] = v
			}
		}
	}
	return out
}

func factsRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "n/a"
		}
		return "err(" + strings.TrimPrefix(err.Error(), "open "+path+": ") + ")"
	}
	return strings.TrimSpace(string(b))
}

func TestUpstreamFacts(t *testing.T) {
	// ---- go ------------------------------------------------------------------------------------------
	t.Logf("FACTS test binary: %s", hostFacts())
	if goBin, err := exec.LookPath("go"); err != nil {
		t.Logf("FACTS go: no go command on PATH: %v", err)
	} else {
		t.Logf("FACTS go on PATH: %s", goBin)
		t.Logf("FACTS inherited env: GOTOOLCHAIN=%q GOFLAGS=%q GOROOT=%q GOEXPERIMENT=%q GODEBUG=%q GOMAXPROCS=%q",
			os.Getenv("GOTOOLCHAIN"), os.Getenv("GOFLAGS"), os.Getenv("GOROOT"), os.Getenv("GOEXPERIMENT"), os.Getenv("GODEBUG"), os.Getenv("GOMAXPROCS"))
		wd, _ := os.Getwd()
		factsRun(t, "go version [host env]", time.Minute, wd, os.Environ(), goBin, "version")
		factsRun(t, "go env [host env]", time.Minute, wd, os.Environ(), goBin, "env")

		var versions []string
		for _, v := range envList("CELERIS_PROBE_GOVERSIONS", "local") {
			if v != "local" {
				versions = append(versions, v)
			}
		}
		if len(versions) > 0 {
			tmp, err := os.MkdirTemp("", "upfacts-")
			if err != nil {
				t.Logf("FACTS go [matrix env]: no temp dir: %v", err)
			} else {
				defer func() { // the module cache and downloaded toolchains are read-only: make them writable first
					_ = filepath.Walk(tmp, func(p string, fi os.FileInfo, err error) error {
						if err == nil && fi.IsDir() {
							_ = os.Chmod(p, 0o755)
						}
						return nil
					})
					_ = os.RemoveAll(tmp)
				}()
				for _, d := range []string{"gopath", "gocache", "gomodcache", "home", "xdg", "src"} {
					_ = os.MkdirAll(filepath.Join(tmp, d), 0o755)
				}
				for _, v := range versions {
					e := upEnv{tmp: tmp, toolchain: "go" + v}
					lbl := " [matrix env GOTOOLCHAIN=go" + v + "]"
					factsRun(t, "go version"+lbl, 10*time.Minute, filepath.Join(tmp, "src"), e.env(), goBin, "version")
					factsRun(t, "go env"+lbl, 10*time.Minute, filepath.Join(tmp, "src"), e.env(), goBin, "env")
				}
			}
		}
	}

	// ---- kernel --------------------------------------------------------------------------------------
	factsRun(t, "uname -a", 30*time.Second, "", os.Environ(), "uname", "-a")

	// ---- cpus ----------------------------------------------------------------------------------------
	const sys = "/sys/devices/system/cpu"
	t.Logf("FACTS cpu sets: online=%s offline=%s possible=%s present=%s",
		factsRead(sys+"/online"), factsRead(sys+"/offline"), factsRead(sys+"/possible"), factsRead(sys+"/present"))
	info := factsCPUInfo()
	ids := map[int]bool{}
	for id := range info {
		ids[id] = true
	}
	if ents, err := os.ReadDir(sys); err == nil {
		re := regexp.MustCompile(`^cpu(\d+)$`)
		for _, e := range ents {
			if m := re.FindStringSubmatch(e.Name()); m != nil {
				n, _ := strconv.Atoi(m[1])
				ids[n] = true
			}
		}
	} else {
		t.Logf("FACTS cpu: reading %s: %v", sys, err)
	}
	var order []int
	for id := range ids {
		order = append(order, id)
	}
	sort.Ints(order)
	if len(order) == 0 {
		t.Logf("FACTS cpu: no cpu found")
	}
	for _, id := range order {
		d := fmt.Sprintf("%s/cpu%d", sys, id)
		ci, ok := info[id]
		var id1 string
		if !ok {
			id1 = "cpuinfo=absent(offline?)"
		} else if _, arm := ci["CPU part"]; arm {
			id1 = fmt.Sprintf("implementer=%s variant=%s part=%s revision=%s architecture=%s",
				ci["CPU implementer"], ci["CPU variant"], ci["CPU part"], ci["CPU revision"], ci["CPU architecture"])
		} else {
			id1 = fmt.Sprintf("vendor=%q family=%s model=%s stepping=%s name=%q",
				ci["vendor_id"], ci["cpu family"], ci["model"], ci["stepping"], ci["model name"])
		}
		t.Logf("FACTS cpu %d: %s midr_el1=%s cpu_capacity=%s scaling_cur_freq=%s cpuinfo_max_freq=%s",
			id, id1, factsRead(d+"/regs/identification/midr_el1"), factsRead(d+"/cpu_capacity"),
			factsRead(d+"/cpufreq/scaling_cur_freq"), factsRead(d+"/cpufreq/cpuinfo_max_freq"))
	}

	// ---- dmesg (best effort) -------------------------------------------------------------------------
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "dmesg")
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput() // dmesg 2>&1
		if err != nil {
			first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			t.Logf("FACTS dmesg: FAILED %v: %s", err, first)
			return
		}
		re := regexp.MustCompile(`(?i)errat|workaround|2966298`) // grep -i -E "errat|workaround|2966298"
		n := 0
		all := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		for _, l := range all {
			if re.MatchString(l) {
				n++
				t.Logf("FACTS dmesg: %s", strings.TrimRight(l, " \t\r"))
			}
		}
		t.Logf("FACTS dmesg: %d of %d lines match errat|workaround|2966298", n, len(all))
	}()

	// ---- lscpu (when present) ------------------------------------------------------------------------
	if p, err := exec.LookPath("lscpu"); err != nil {
		t.Logf("FACTS lscpu: not present")
	} else {
		factsRun(t, "lscpu", 30*time.Second, "", os.Environ(), p)
	}
}
