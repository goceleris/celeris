//go:build linux

package stallprobe

// T8 tip support: CELERIS_PROBE_GOVERSIONS=tip builds the Go toolchain from source (src/make.bash) and
// TestUpstreamMatrix then treats it like any other version. Nothing here may fail the test: every
// unreachable network, missing tool or failed build is a MATRIX_SKIP version=tip row.
//
// Knobs (celeris-stress extra=NAME=V):
//
//	CELERIS_PROBE_TIP_REF        branch, tag or 40-char sha of golang/go (master). It is resolved to a full sha
//	                             BEFORE anything is built, and the sha is logged (MATRIX_FACTS tip resolved).
//	CELERIS_PROBE_TIP_NOGIT      any value: ignore git and use the GitHub API + codeload tarball path (a test knob)
//	CELERIS_PROBE_TIP_BOOTSTRAP  optional release (e.g. 1.26.5) whose GOROOT is the bootstrap, fetched through the
//	                             existing GOTOOLCHAIN=go<ver> mechanism; default is the GOROOT of the go on PATH.
//
// Source: git (go.googlesource.com/go, blobless clone, then plain clone) when git exists, else (or when git fails)
// the github.com/golang/go codeload tarball of the resolved sha. Build: GOROOT_BOOTSTRAP, CGO_ENABLED=0,
// GOROOT_FINAL unset, everything inside the harness temp dir, 40 min limit. Patch arms are never run for tip
// (their anchors are absent on master).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	tipGitURL    = "https://go.googlesource.com/go"
	tipGitHubAPI = "https://api.github.com/repos/golang/go/commits/"
	tipCodeload  = "https://codeload.github.com/golang/go/tar.gz/"
)

var (
	tipRefRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./+-]*$`)
	tipShaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func tipHTTPGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "celeris-stallprobe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// tipAPICommit asks the GitHub API for the commit sha and committer date of ref.
func tipAPICommit(ctx context.Context, ref string) (sha, date string, err error) {
	resp, err := tipHTTPGet(ctx, tipGitHubAPI+ref)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var j struct {
		Sha    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&j); err != nil {
		return "", "", err
	}
	if !tipShaRe.MatchString(j.Sha) {
		return "", "", fmt.Errorf("github API returned sha %q", j.Sha)
	}
	return j.Sha, j.Commit.Committer.Date, nil
}

// tipResolve turns ref into a full 40-char sha (ls-remote on go.googlesource.com, else the GitHub API).
func tipResolve(ctx context.Context, e upEnv, ref string, haveGit bool) (sha string, why string) {
	if !tipRefRe.MatchString(ref) {
		return "", "CELERIS_PROBE_TIP_REF " + strconv.Quote(ref) + " is not a plain ref name or sha"
	}
	if tipShaRe.MatchString(ref) {
		return ref, ""
	}
	var errs []string
	if haveGit {
		out, se, err := upRun(ctx, e.tmp, append(e.env(), "GIT_TERMINAL_PROMPT=0"), "git", "ls-remote", tipGitURL,
			"refs/heads/"+ref, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
		if err != nil {
			errs = append(errs, "git ls-remote "+tipGitURL+": "+err.Error()+" "+tail(se, 200))
		} else {
			best, rank := "", 0
			for _, l := range strings.Split(out, "\n") {
				f := strings.Fields(l)
				if len(f) != 2 || !tipShaRe.MatchString(f[0]) {
					continue
				}
				r := 1 // lightweight tag
				switch {
				case f[1] == "refs/heads/"+ref:
					r = 3
				case strings.HasSuffix(f[1], "^{}"):
					r = 2
				}
				if r > rank {
					best, rank = f[0], r
				}
			}
			if best != "" {
				return best, ""
			}
			errs = append(errs, "git ls-remote "+tipGitURL+": no ref "+ref)
		}
	} else {
		errs = append(errs, "no git on PATH")
	}
	s, _, err := tipAPICommit(ctx, ref)
	if err == nil {
		return s, ""
	}
	errs = append(errs, "github API: "+err.Error())
	return "", strings.Join(errs, "; ")
}

// tipFetch puts the source of commit sha in dir. how says which path worked.
func tipFetch(ctx context.Context, t *testing.T, e upEnv, dir, sha string, haveGit bool) (how string, errs []string) {
	genv := append(e.env(), "GIT_TERMINAL_PROMPT=0")
	if haveGit {
		for _, mode := range [][]string{{"--filter=blob:none", "--no-checkout"}, {"--no-checkout"}} {
			_ = os.RemoveAll(dir)
			args := append(append([]string{"clone", "--quiet"}, mode...), tipGitURL, dir)
			if _, se, err := upRun(ctx, e.tmp, genv, "git", args...); err != nil {
				errs = append(errs, "git "+strings.Join(mode, " ")+" clone: "+err.Error()+" "+tail(se, 200))
				continue
			}
			if _, se, err := upRun(ctx, dir, genv, "git", "checkout", "--quiet", "--detach", sha); err != nil {
				errs = append(errs, "git checkout "+sha+": "+err.Error()+" "+tail(se, 200))
				continue
			}
			return "git clone " + tipGitURL + " (" + strings.Join(mode, " ") + ")", errs
		}
	}
	_ = os.RemoveAll(dir)
	if err := tipUntar(ctx, tipCodeload+sha, dir); err != nil {
		return "", append(errs, "codeload tarball: "+err.Error())
	}
	return "github codeload tarball of " + sha, errs
}

// tipUntar downloads a .tar.gz and extracts it into dir, dropping the first path component. The archive is
// untrusted: names that leave dir are refused.
func tipUntar(ctx context.Context, url, dir string) error {
	resp, err := tipHTTPGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root := filepath.Clean(dir) + string(filepath.Separator)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, ok := strings.Cut(h.Name, "/")
		if !ok || rel == "" {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(dst+string(filepath.Separator), root) {
			return fmt.Errorf("tar entry %q escapes the target directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// upPrepareTip resolves, fetches and builds Go tip inside tmp. It returns the path of the new go command, or ""
// after logging a MATRIX_SKIP version=tip (never a test failure).
func upPrepareTip(t *testing.T, goBin, tmp string) (tipGo string) {
	skip := func(format string, a ...any) string {
		t.Logf("MATRIX_SKIP version=tip reason=%q", fmt.Sprintf(format, a...))
		return ""
	}
	if len(envList("CELERIS_PROBE_PATCH", "")) > 0 {
		return skip("patch arms are not run for tip (their anchors are absent on master); unset CELERIS_PROBE_PATCH")
	}
	e := upEnv{tmp: tmp, toolchain: "local"}
	ref := "master"
	if l := envList("CELERIS_PROBE_TIP_REF", "master"); len(l) > 0 {
		ref = l[0]
	}
	_, gitErr := lookGit()
	haveGit := gitErr == nil && os.Getenv("CELERIS_PROBE_TIP_NOGIT") == "" // NOGIT=1 exercises the tarball path
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Minute)
	sha, why := tipResolve(rctx, e, ref, haveGit)
	apiDate := ""
	if sha != "" {
		if _, d, err := tipAPICommit(rctx, sha); err == nil {
			apiDate = d
		}
	}
	rcancel()
	if sha == "" {
		return skip("cannot resolve CELERIS_PROBE_TIP_REF=%s: %s", ref, why)
	}
	t.Logf("MATRIX_FACTS tip resolved: ref=%s sha=%s", ref, sha)

	// bootstrap
	bootGo, bootTC := goBin, "local"
	if bv := strings.TrimPrefix(os.Getenv("CELERIS_PROBE_TIP_BOOTSTRAP"), "go"); bv != "" {
		bootTC = "go" + bv
	}
	be := upEnv{tmp: tmp, toolchain: bootTC}
	bctx, bcancel := context.WithTimeout(context.Background(), 10*time.Minute)
	bootRoot, se, err := upRun(bctx, tmp, be.env(), bootGo, "env", "GOROOT")
	bcancel()
	bootRoot = strings.TrimSpace(bootRoot)
	if err != nil || bootRoot == "" {
		return skip("cannot determine the bootstrap GOROOT (toolchain %s): %v %s", bootTC, err, tail(se, 300))
	}
	t.Logf("MATRIX_FACTS tip bootstrap: GOROOT_BOOTSTRAP=%s toolchain=%s", bootRoot, bootTC)

	// source
	dir := filepath.Join(tmp, "tipgo")
	fctx, fcancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t0 := time.Now()
	how, ferrs := tipFetch(fctx, t, e, dir, sha, haveGit)
	fcancel()
	for _, m := range ferrs {
		t.Logf("MATRIX_NOTE tip fetch attempt failed: %s", m)
	}
	if how == "" {
		return skip("cannot fetch the Go source of %s: %s", sha, strings.Join(ferrs, "; "))
	}
	t.Logf("MATRIX_FACTS tip source: %s in %.0f s", how, time.Since(t0).Seconds())
	date := apiDate
	if haveGit {
		if o, _, err := upRun(context.Background(), dir, e.env(), "git", "show", "-s", "--format=%cI", "HEAD"); err == nil && strings.TrimSpace(o) != "" {
			date = strings.TrimSpace(o)
		}
	}
	t.Logf("MATRIX_FACTS tip commit: sha=%s date=%s", sha, date)
	if _, err := os.Stat(filepath.Join(dir, "VERSION")); err != nil && !pathExists(filepath.Join(dir, ".git")) {
		// a tarball has neither .git nor VERSION and cmd/dist insists on one of them: write a devel VERSION
		minor := ""
		if b, err := os.ReadFile(filepath.Join(dir, "src/internal/goversion/goversion.go")); err == nil {
			if m := regexp.MustCompile(`(?m)^const Version = (\d+)`).FindSubmatch(b); m != nil {
				minor = string(m[1])
			}
		}
		if minor == "" {
			return skip("tarball source without .git: cannot read internal/goversion.Version to write a VERSION file")
		}
		ver := "go1." + minor + "-devel_" + sha[:12]
		_ = os.WriteFile(filepath.Join(dir, "VERSION"), []byte(ver+"\ntime "+date+"\n"), 0o644)
		t.Logf("MATRIX_FACTS tip wrote VERSION file for the tarball: %s", ver)
	}

	// build
	var menv []string
	for _, kv := range e.env() {
		if !strings.HasPrefix(kv, "GOROOT_FINAL=") && !strings.HasPrefix(kv, "GOROOT_BOOTSTRAP=") && !strings.HasPrefix(kv, "GOFLAGS=") {
			menv = append(menv, kv)
		}
	}
	menv = append(menv, "GOROOT_BOOTSTRAP="+bootRoot, "GOFLAGS=", "CGO_ENABLED=0")
	mctx, mcancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t1 := time.Now()
	mo, mse, err := upRun(mctx, filepath.Join(dir, "src"), menv, "bash", "make.bash")
	mcancel()
	t.Logf("MATRIX_FACTS tip make.bash: %.0f s err=%v tail=%q", time.Since(t1).Seconds(), err, tail(mo+"\n"+mse, 400))
	if err != nil {
		return skip("make.bash failed: %v %s", err, tail(mo+"\n"+mse, 600))
	}
	tipGo = filepath.Join(dir, "bin", "go")
	tipFacts(t, dir, tipGo, e, haveGit)
	return tipGo
}

func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

func lookGit() (string, error) { return exec.LookPath("git") }

var tipTokens = []string{"nextPreemptM", "yieldDelay", "preemptGen", "signalPending"}
var tipAssignRe = regexp.MustCompile(`\bnextPreemptM\s*=[^=]`)

// tipFacts logs what is needed to state exactly whether the 5 us resend pacing is unchanged: file hashes, the
// line numbers (and text) of the pacing and ack tokens, the lines around each nextPreemptM assignment, the git history.
func tipFacts(t *testing.T, dir, tipGo string, e upEnv, haveGit bool) {
	if b, err := os.ReadFile(filepath.Join(dir, "VERSION")); err == nil {
		t.Logf("MATRIX_FACTS tip $GOROOT/VERSION: %q", strings.TrimSpace(string(b)))
	} else {
		t.Logf("MATRIX_FACTS tip $GOROOT/VERSION: absent (%v)", err)
	}
	gv, _, _ := upRun(context.Background(), dir, e.env(), tipGo, "env", "GOVERSION")
	t.Logf("MATRIX_FACTS tip go env GOVERSION: %q", strings.TrimSpace(gv))
	for _, name := range []string{"preempt.go", "signal_unix.go"} {
		b, err := os.ReadFile(filepath.Join(dir, "src/runtime", name))
		if err != nil {
			t.Logf("MATRIX_FACTS tip file %s: %v", name, err)
			continue
		}
		lines := strings.Split(string(b), "\n")
		t.Logf("MATRIX_FACTS tip file %s sha256=%x bytes=%d lines=%d", name, sha256.Sum256(b), len(b), len(lines))
		for _, tok := range tipTokens {
			var nums []string
			for i, l := range lines {
				if strings.Contains(l, tok) {
					nums = append(nums, strconv.Itoa(i+1))
				}
			}
			t.Logf("MATRIX_FACTS tip file %s token %s lines: %s", name, tok, strings.Join(nums, ","))
			for i, l := range lines {
				if strings.Contains(l, tok) {
					t.Logf("MATRIX_FACTS tip line %s:%d: %s", name, i+1, strings.TrimSpace(l))
				}
			}
		}
		if name == "preempt.go" {
			for i, l := range lines {
				if !tipAssignRe.MatchString(l) {
					continue
				}
				for j := max(0, i-5); j <= min(len(lines)-1, i+4); j++ {
					t.Logf("MATRIX_FACTS tip ctx %s:%d: %s", name, j+1, strings.ReplaceAll(lines[j], "\t", "    "))
				}
			}
		}
	}
	if haveGit {
		for _, name := range []string{"preempt.go", "signal_unix.go"} {
			o, se, err := upRun(context.Background(), dir, e.env(), "git", "log", "-5", "--date=short", "--format=%h %ad %s", "--", "src/runtime/"+name)
			t.Logf("MATRIX_FACTS tip git log -5 %s: err=%v %s", name, err, tail(o+se, 1500))
		}
	}
}
