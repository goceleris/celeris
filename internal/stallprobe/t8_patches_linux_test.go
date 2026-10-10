//go:build linux

package stallprobe

// Runtime patches for TestUpstreamMatrix (CELERIS_PROBE_PATCH=<name>[,<name>...]). EXPERIMENT ONLY: each
// patch changes how runtime.suspendG (src/runtime/preempt.go) spaces its SIGURG resends so the
// H1 mechanism can be tested; none of them is a proposal for merging.
//
// How they are applied without touching the toolchain: the patch function reads GOROOT/src/runtime/preempt.go
// of the toolchain under test (read-only), edits it with exact-anchor string replacements (an anchor that is
// missing or not unique is an error, so a toolchain whose preempt.go differs is skipped, never silently run
// unpatched), writes the result into the scratch dir and returns a `go build -overlay` JSON mapping the
// original path to the copy. The Go command compiles the runtime package from the overlay file; the harness
// then logs the size of runtime.suspendG in the built binary (`go tool nm -size`) so a patched arm is visibly
// a different function, and the noop arm has the same size as the unpatched one.
//
// The patches use no min/max builtins, so they also build on toolchains older than go1.21.
//
//	noop            identical bytes. CONTROL: the overlay mechanism alone must not change behaviour
//	backoff         resend delay doubles on every resend (5us, 10us, 20us ... capped at 1 ms)
//	min100us        resend delay is a fixed 100 us instead of yieldDelay/2 (5 us)
//	acklatency      a resend waits at least as long as the previous send took to be acknowledged
//	cl842745        the change of Gerrit CL 842745 (runtime: start preemption retry delay after each attempt):
//	                nextPreemptM = nanotime() + yieldDelay/2 is assigned AFTER preemptM returns instead of
//	                nextPreemptM = now + yieldDelay/2 before it; the delay stays 5 us. The CL's diff is against a
//	                master whose loop reads preemptM(gp)/break; the released toolchains read preemptM(asyncM), so
//	                the same two-line move is applied at the released-toolchain anchor (ppSendAnchor).
//	negctl-buildfail NEGATIVE CONTROL: injects an undefined symbol; the build MUST fail (MATRIX_SKIP). Proves the overlay
//	                reaches the runtime compile. Never use it in a measurement.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ppDeclAnchor = "\tvar nextPreemptM int64\n"
	ppSendAnchor = "\t\t\t\t\tnextPreemptM = now + yieldDelay/2\n\t\t\t\t\tpreemptM(asyncM)\n"
	ppGenAnchor  = "\t\t\tneedAsync := asyncM != asyncM2 || asyncGen != asyncGen2\n"
)

// ppReplace replaces old by new exactly once or fails.
func ppReplace(src, old, new, what string) (string, error) {
	if n := strings.Count(src, old); n != 1 {
		return "", fmt.Errorf("anchor %q found %d times in preempt.go (want 1)", what, n)
	}
	return strings.Replace(src, old, new, 1), nil
}

func ppRegister(name string, edit func(src string) (string, error)) {
	upstreamPatches[name] = func(goroot, scratch string) (string, error) {
		orig := filepath.Join(goroot, "src/runtime/preempt.go")
		b, err := os.ReadFile(orig)
		if err != nil {
			return "", err
		}
		out, err := edit(string(b))
		if err != nil {
			return "", err
		}
		dst := filepath.Join(scratch, "preempt.go")
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return "", err
		}
		j, _ := json.Marshal(map[string]any{"Replace": map[string]string{orig: dst}})
		ov := filepath.Join(scratch, "overlay.json")
		return ov, os.WriteFile(ov, j, 0o644)
	}
}

func init() {
	ppRegister("noop", func(src string) (string, error) { return src, nil })

	ppRegister("backoff", func(src string) (string, error) {
		s, err := ppReplace(src, ppDeclAnchor, ppDeclAnchor+"\tpreemptMDelay := int64(yieldDelay / 2)\n", "var nextPreemptM")
		if err != nil {
			return "", err
		}
		return ppReplace(s, ppSendAnchor,
			"\t\t\t\t\tnextPreemptM = now + preemptMDelay\n"+
				"\t\t\t\t\t// EXPERIMENT: a resend means the previous signal was handled while gp stayed at an\n"+
				"\t\t\t\t\t// unsafe point. Back off so a slow signal path (27us on a Cortex-A520) cannot be re-hit\n"+
				"\t\t\t\t\t// before sigreturn. Cap 1 ms.\n"+
				"\t\t\t\t\tpreemptMDelay *= 2\n"+
				"\t\t\t\t\tif preemptMDelay > 1000*1000 {\n"+
				"\t\t\t\t\t\tpreemptMDelay = 1000 * 1000\n"+
				"\t\t\t\t\t}\n"+
				"\t\t\t\t\tpreemptM(asyncM)\n", "nextPreemptM = now + yieldDelay/2 ... preemptM(asyncM)")
	})

	ppRegister("min100us", func(src string) (string, error) {
		return ppReplace(src, ppSendAnchor,
			"\t\t\t\t\t// EXPERIMENT: fixed 100us resend floor instead of yieldDelay/2.\n"+
				"\t\t\t\t\tnextPreemptM = now + 100*1000\n\t\t\t\t\tpreemptM(asyncM)\n", "nextPreemptM = now + yieldDelay/2 ... preemptM(asyncM)")
	})

	ppRegister("acklatency", func(src string) (string, error) {
		s, err := ppReplace(src, ppDeclAnchor, ppDeclAnchor+"\tvar lastPreemptM int64\n", "var nextPreemptM")
		if err != nil {
			return "", err
		}
		s, err = ppReplace(s, ppGenAnchor, ppGenAnchor+
			"\t\t\tif asyncM == asyncM2 && asyncGen != asyncGen2 && lastPreemptM != 0 {\n"+
			"\t\t\t\t// EXPERIMENT: the previous signal was handled without stopping gp. Let gp run at\n"+
			"\t\t\t\t// least as long as the signal took (send to observed ack) before signalling again.\n"+
			"\t\t\t\tnow := nanotime()\n"+
			"\t\t\t\tif d := now + (now - lastPreemptM); d > nextPreemptM {\n"+
			"\t\t\t\t\tnextPreemptM = d\n"+
			"\t\t\t\t}\n"+
			"\t\t\t}\n", "needAsync :=")
		if err != nil {
			return "", err
		}
		return ppReplace(s, ppSendAnchor, "\t\t\t\t\tnextPreemptM = now + yieldDelay/2\n\t\t\t\t\tlastPreemptM = now\n\t\t\t\t\tpreemptM(asyncM)\n", "send")
	})

	ppRegister("cl842745", func(src string) (string, error) {
		return ppReplace(src, ppSendAnchor,
			"\t\t\t\t\tpreemptM(asyncM)\n"+
				"\t\t\t\t\t// CL 842745: start the delay after preemptM returns, so the attempt itself does not count as waiting time.\n"+
				"\t\t\t\t\tnextPreemptM = nanotime() + yieldDelay/2\n", "nextPreemptM = now + yieldDelay/2 ... preemptM(asyncM)")
	})

	ppRegister("negctl-buildfail", func(src string) (string, error) {
		return ppReplace(src, ppSendAnchor, "\t\t\t\t\tnextPreemptM = undefinedSymbolControl\n\t\t\t\t\tpreemptM(asyncM)\n", "send")
	})
}
