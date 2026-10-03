package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// The build matrix: every package is loaded once per platform and tag set.
var (
	platforms = []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"}
	tagSets   = []string{"", "validation", "celeris_closeprobe"}
)

// config is one entry of the matrix; its index is platform*len(tagSets)+tags.
type config struct {
	index        int
	goos, goarch string
	tags         string
}

func configs() []config {
	cs := make([]config, 0, len(platforms)*len(tagSets))
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		for _, t := range tagSets {
			cs = append(cs, config{index: len(cs), goos: goos, goarch: goarch, tags: t})
		}
	}
	return cs
}

// env is the environment go list runs in: the caller's, with the target
// platform, cgo off (as the #443 inventory had it, and so the host's C
// toolchain cannot change the result), no workspace and no inherited
// GOFLAGS.
func (c config) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOWORK":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOOS="+c.goos, "GOARCH="+c.goarch, "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOWORK=off")
}

func (c config) buildFlags() []string {
	if c.tags == "" {
		return nil
	}
	return []string{"-tags=" + c.tags}
}

// module is one Go module whose packages get golden files.
type module struct {
	dir, path string
}

// findModules returns the root module and every module nested below it
// whose path extends the root module's path and whose directory has no
// test, testdata or internal element (test/benchcmp_ws and the like are
// test harnesses, not API).
func findModules(root, rootPath string) ([]module, error) {
	mods := []module{{dir: root, path: rootPath}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
			return fs.SkipDir
		}
		mp, err := modulePath(filepath.Join(p, "go.mod"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(mp, rootPath+"/") && !excludedPath(filepath.ToSlash(rel)) {
			mods = append(mods, module{dir: p, path: mp})
		}
		return nil
	})
	return mods, err
}

// excludedPath reports whether a slash path (an import path below the root
// module, or a directory) is outside the API: internal packages, test
// harnesses and test data.
func excludedPath(rel string) bool {
	for _, e := range strings.Split(rel, "/") {
		switch e {
		case "internal", "test", "testdata":
			return true
		}
	}
	return false
}

// pkgAPI is what one package contributes across the matrix.
type pkgAPI struct {
	path    string
	present uint32            // configurations the package builds in
	facts   map[string]uint32 // line -> configurations it holds in
}

// generate loads every module in every configuration and renders the golden
// files, keyed by slash path relative to the root.
func generate(root string) (map[string][]byte, error) {
	rootPath, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	mods, err := findModules(root, rootPath)
	if err != nil {
		return nil, err
	}
	cs := configs()
	ck := newChecker()

	var (
		mu    sync.Mutex
		apis  = map[string]*pkgAPI{}
		owner = map[string]string{} // import path -> module dir, to catch a path two modules claim
		errs  []error
		wg    sync.WaitGroup
		sem   = make(chan struct{}, max(runtime.GOMAXPROCS(0), 2))
	)
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, m := range mods {
		for _, c := range cs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				roots, err := packages.Load(&packages.Config{
					Mode:       packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps,
					Dir:        m.dir,
					Env:        c.env(),
					BuildFlags: c.buildFlags(),
				}, "./...")
				<-sem
				if err != nil {
					fail(fmt.Errorf("%s, %s/%s %s: %v", m.path, c.goos, c.goarch, c.buildFlags(), err))
					return
				}
				for _, p := range roots {
					if !inAPI(rootPath, p) {
						continue
					}
					if err := listErr(p); err != nil {
						if isNoGoFiles(err) {
							continue
						}
						fail(fmt.Errorf("%s/%s %s: %v", c.goos, c.goarch, c.buildFlags(), err))
						continue
					}
					if len(p.GoFiles) == 0 {
						continue
					}
					tp, err := ck.check(p, c)
					if err != nil {
						fail(fmt.Errorf("%s/%s %s: %v", c.goos, c.goarch, c.buildFlags(), err))
						continue
					}
					lines := extract(tp, rootPath)
					mu.Lock()
					if o, ok := owner[p.PkgPath]; ok && o != m.dir {
						errs = append(errs, fmt.Errorf("%s is in two modules: %s and %s", p.PkgPath, o, m.dir))
					}
					owner[p.PkgPath] = m.dir
					a := apis[p.PkgPath]
					if a == nil {
						a = &pkgAPI{path: p.PkgPath, facts: map[string]uint32{}}
						apis[p.PkgPath] = a
					}
					a.present |= 1 << c.index
					for _, l := range lines {
						a.facts[l] |= 1 << c.index
					}
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(errs) > 0 {
		// A package that fails to type-check is reported by every package
		// that imports it, so print each message once.
		seen := map[string]bool{}
		var msgs []string
		for _, e := range errs {
			if m := e.Error(); !seen[m] {
				seen[m] = true
				msgs = append(msgs, m)
			}
		}
		sort.Strings(msgs)
		return nil, errors.New(strings.Join(msgs, "\n"))
	}
	files := map[string][]byte{}
	for _, a := range apis {
		name := fileName(rootPath, a.path)
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("two packages map to %s", name)
		}
		files[name] = render(a)
	}
	return files, nil
}

// inAPI reports whether a package listed by ./... gets a golden file.
func inAPI(rootPath string, p *packages.Package) bool {
	if p.Name == "main" {
		return false
	}
	if p.PkgPath != rootPath && !strings.HasPrefix(p.PkgPath, rootPath+"/") {
		return false
	}
	return !excludedPath(strings.TrimPrefix(strings.TrimPrefix(p.PkgPath, rootPath), "/"))
}

// listErr returns the first error go list reported for the package itself.
func listErr(p *packages.Package) error {
	for _, e := range p.Errors {
		return e
	}
	return nil
}

// isNoGoFiles reports a package that has no Go files in this configuration
// (its build constraints exclude every file): it is absent there.
func isNoGoFiles(err error) bool {
	return strings.Contains(err.Error(), "build constraints exclude all Go files")
}

// checker type-checks packages from source, declarations only (function
// bodies are skipped), and shares the result between configurations whose
// inputs are identical: a package's key is its platform, its files and the
// keys of its imports, so the standard library is checked once per
// platform, not once per tag set and module.
type checker struct {
	fset  *token.FileSet
	mu    sync.Mutex
	files map[string]*parsed
	pkgs  map[string]*checked
	keys  map[*packages.Package]string
}

type parsed struct {
	once sync.Once
	file *ast.File
	err  error
}

type checked struct {
	once sync.Once
	pkg  *types.Package
	err  error
}

func newChecker() *checker {
	return &checker{
		fset:  token.NewFileSet(),
		files: map[string]*parsed{},
		pkgs:  map[string]*checked{},
		keys:  map[*packages.Package]string{},
	}
}

func (ck *checker) key(p *packages.Package, c config) string {
	ck.mu.Lock()
	k, ok := ck.keys[p]
	ck.mu.Unlock()
	if ok {
		return k
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s\x00%s\x00", c.goos, c.goarch, p.PkgPath)
	files := append([]string(nil), p.GoFiles...)
	sort.Strings(files)
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00", f)
	}
	imps := make([]string, 0, len(p.Imports))
	for path := range p.Imports {
		imps = append(imps, path)
	}
	sort.Strings(imps)
	for _, path := range imps {
		fmt.Fprintf(&b, "%s=%s\x00", path, ck.key(p.Imports[path], c))
	}
	sum := sha256.Sum256([]byte(b.String()))
	k = hex.EncodeToString(sum[:])
	ck.mu.Lock()
	ck.keys[p] = k
	ck.mu.Unlock()
	return k
}

// check returns the type-checked package, checking its imports first. The
// import graph has no cycles, so the nested once.Do calls cannot deadlock.
func (ck *checker) check(p *packages.Package, c config) (*types.Package, error) {
	if p.PkgPath == "unsafe" {
		return types.Unsafe, nil
	}
	k := ck.key(p, c)
	ck.mu.Lock()
	e := ck.pkgs[k]
	if e == nil {
		e = &checked{}
		ck.pkgs[k] = e
	}
	ck.mu.Unlock()
	e.once.Do(func() { e.pkg, e.err = ck.typecheck(p, c) })
	return e.pkg, e.err
}

func (ck *checker) typecheck(p *packages.Package, c config) (*types.Package, error) {
	if err := listErr(p); err != nil {
		return nil, err
	}
	deps := make(map[string]*types.Package, len(p.Imports))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		derr error
	)
	for path, dep := range p.Imports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tp, err := ck.check(dep, c)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && derr == nil {
				derr = err
			}
			deps[path] = tp
		}()
	}
	files := make([]*ast.File, len(p.GoFiles))
	for i, name := range p.GoFiles {
		f, err := ck.parse(name)
		if err != nil {
			wg.Wait()
			return nil, err
		}
		files[i] = f
	}
	wg.Wait()
	if derr != nil {
		return nil, derr
	}
	var terrs []string
	conf := types.Config{
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if tp := deps[path]; tp != nil {
				return tp, nil
			}
			return nil, fmt.Errorf("%s: import %q was not listed", p.PkgPath, path)
		}),
		Sizes:                    types.SizesFor("gc", c.goarch),
		IgnoreFuncBodies:         true,
		DisableUnusedImportCheck: true,
		Error: func(err error) {
			if len(terrs) < 10 {
				terrs = append(terrs, err.Error())
			}
		},
	}
	tp, _ := conf.Check(p.PkgPath, ck.fset, files, nil)
	if len(terrs) > 0 {
		return nil, fmt.Errorf("type-checking %s:\n\t%s", p.PkgPath, strings.Join(terrs, "\n\t"))
	}
	return tp, nil
}

// parse parses a file once for all configurations. Function bodies are
// emptied, since the checker ignores them and only declarations matter.
func (ck *checker) parse(name string) (*ast.File, error) {
	ck.mu.Lock()
	e := ck.files[name]
	if e == nil {
		e = &parsed{}
		ck.files[name] = e
	}
	ck.mu.Unlock()
	e.once.Do(func() {
		e.file, e.err = parser.ParseFile(ck.fset, name, nil, parser.SkipObjectResolution)
		if e.file != nil {
			for _, d := range e.file.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					fd.Body.List = nil
				}
			}
		}
	})
	return e.file, e.err
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
