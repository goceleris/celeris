// Command apidump keeps the API golden files under api/ in step with the
// exported API of every celeris package (celeris#443).
//
// It writes one file per importable package, api/<path>.txt, where <path> is
// the import path below the root module ("celeris" for the root package
// itself). A file lists every exported identifier of its package with its
// full signature, one per line: consts with their values, vars, funcs, types
// with their fields, interface methods and method sets (promoted ones
// included), and type parameters written as $0, $1, ... The lines are sorted.
//
// The packages are those of the root module and of every nested module whose
// path is below it (middleware/compress, metrics, otel and protobuf), minus
// package main and every path with an internal, test or testdata element.
// Each package is loaded for linux/amd64, linux/arm64, darwin/arm64 and
// windows/amd64, each with no tag, with -tags=validation and with
// -tags=celeris_closeprobe (cgo off): twelve configurations, the build matrix
// of the #443 inventory. A file's header names the configurations its
// package builds in, and a line that holds in only some of them ends in
// "// only: ..." naming them.
//
// A type that the exported API reaches but that has no file of its own (an
// unexported type, or one from an internal package) is listed too, as
// "exposed type ...", since its fields and methods are usable through that
// API.
//
// Usage, from the repository root:
//
//	go -C .github/tools run ./apidump      # check: print the diff and exit 1 if api/ is stale
//	go -C .github/tools run ./apidump -w   # rewrite api/
//
// `mage api` runs the second command. CI runs the first.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// apiDir is where the golden files live, relative to the repository root.
const apiDir = "api"

const regenerate = "mage api   (or: go -C .github/tools run ./apidump -w)"

func main() {
	write := flag.Bool("w", false, "rewrite the files under api/ instead of checking them")
	root := flag.String("root", "", "repository root (default: the nearest directory at or above the working directory\nwhose go.mod is not this tool's own)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: apidump [-w] [-root dir]\n\n"+
			"Without -w, apidump checks that api/ matches the exported API and exits 1\n"+
			"with the diff when it does not. With -w it rewrites api/.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	dir, err := findRoot(*root)
	if err != nil {
		fatal(err)
	}
	start := time.Now()
	files, err := generate(dir)
	if err != nil {
		fatal(err)
	}
	took := time.Since(start).Round(10 * time.Millisecond)
	if *write {
		changed, err := writeFiles(dir, files)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "apidump: %s/ holds %d packages; %d files changed (%s)\n", apiDir, len(files), changed, took)
		return
	}
	stale, err := compare(dir, files)
	if err != nil {
		fatal(err)
	}
	if len(stale) == 0 {
		fmt.Fprintf(os.Stderr, "apidump: %s/ matches the exported API of %d packages (%s)\n", apiDir, len(files), took)
		return
	}
	if err := report(os.Stdout, stale); err != nil {
		fatal(err)
	}
	os.Exit(1)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "apidump: %v\n", err)
	os.Exit(1)
}

// findRoot returns the repository root: flagRoot if set, else the nearest
// directory at or above the working directory whose go.mod declares a
// module other than this tool's own (so `go -C .github/tools run ./apidump`
// finds the repository root above .github/tools).
func findRoot(flagRoot string) (string, error) {
	if flagRoot != "" {
		return filepath.Abs(flagRoot)
	}
	self := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		self = bi.Main.Path
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if p, err := modulePath(filepath.Join(dir, "go.mod")); err == nil && p != self {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found at or above the working directory; pass -root")
		}
		dir = parent
	}
}

// modulePath reads the module path from a go.mod file.
func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module line", gomod)
}

// fileName maps an import path to its golden file, relative to the root.
func fileName(modPath, importPath string) string {
	rel := strings.TrimPrefix(importPath, modPath)
	if rel == "" {
		rel = "/" + modPath[strings.LastIndex(modPath, "/")+1:]
	}
	return filepath.ToSlash(filepath.Join(apiDir, rel[1:]+".txt"))
}

// writeFiles makes api/ hold exactly files: it rewrites the ones that
// differ and removes .txt files of packages that no longer exist. It returns
// how many files it created, changed or removed.
func writeFiles(root string, files map[string][]byte) (int, error) {
	changed := 0
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return changed, err
		}
		changed++
	}
	existing, err := goldenFiles(root)
	if err != nil {
		return changed, err
	}
	for _, name := range existing {
		if _, ok := files[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			return changed, err
		}
		changed++
	}
	// Drop directories the removals left empty, deepest first.
	var dirs []string
	_ = filepath.WalkDir(filepath.Join(root, apiDir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs[:max(len(dirs)-1, 0)] { // never the api/ directory itself
		if ents, err := os.ReadDir(d); err == nil && len(ents) == 0 {
			_ = os.Remove(d)
		}
	}
	return changed, nil
}

// goldenFiles lists the .txt files under api/, as slash paths relative to
// the root. Other files there (api/README.md) are not apidump's.
func goldenFiles(root string) ([]string, error) {
	var names []string
	base := filepath.Join(root, apiDir)
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == base {
				return fs.SkipDir
			}
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".txt") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(names)
	return names, err
}

// staleFile is one golden file that does not match the source.
type staleFile struct {
	name                 string
	committed, generated []byte // nil when the file is missing on that side
}

// compare returns the golden files that differ from files, sorted by name.
func compare(root string, files map[string][]byte) ([]staleFile, error) {
	var stale []staleFile
	existing, err := goldenFiles(root)
	if err != nil {
		return nil, err
	}
	for _, name := range existing {
		if _, ok := files[name]; !ok {
			committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
			if err != nil {
				return nil, err
			}
			stale = append(stale, staleFile{name: name, committed: committed})
		}
	}
	for name, data := range files {
		committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			stale = append(stale, staleFile{name: name, generated: data})
		case err != nil:
			return nil, err
		case !bytes.Equal(committed, data):
			stale = append(stale, staleFile{name: name, committed: committed, generated: data})
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].name < stale[j].name })
	return stale, nil
}

// report prints what is stale, the diff of each file, and how to fix it. On
// GitHub Actions it also annotates each stale file and writes the diff to
// the job summary.
func report(w io.Writer, stale []staleFile) error {
	gha := os.Getenv("GITHUB_ACTIONS") == "true"
	var out, diffs strings.Builder
	fmt.Fprintf(&out, "apidump: the exported API changed, and %d file(s) under %s/ do not match it.\n\n", len(stale), apiDir)
	for _, s := range stale {
		d := unifiedDiff(s.name, s.committed, s.generated)
		diffs.WriteString(d)
		out.WriteString(d)
		if gha {
			what := "is stale"
			switch {
			case s.committed == nil:
				what = "is missing (a new package)"
			case s.generated == nil:
				what = "names a package that no longer exists"
			}
			fmt.Fprintf(&out, "::error file=%s,title=API golden file %s::Run %s and commit %s/\n", s.name, what, regenerate, apiDir)
		}
	}
	fmt.Fprintf(&out, "\nEvery change to an exported identifier updates %s/ in the same pull request,\n"+
		"so the API change shows in the review. Regenerate with\n\n    %s\n\nand commit the result.\n", apiDir, regenerate)
	if _, err := io.WriteString(w, out.String()); err != nil {
		return err
	}
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); gha && p != "" {
		summary := fmt.Sprintf("### API golden files are stale\n\nRun `%s` and commit `%s/`.\n\n```diff\n%s```\n", regenerate, apiDir, diffs.String())
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(summary); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	return nil
}
