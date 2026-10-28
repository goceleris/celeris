package ctxkit

import (
	"path/filepath"
	"strings"
)

// PathWithin reports whether p is base or a path below it. Both are cleaned
// paths (filepath.Clean), of the same kind: both relative or both absolute.
//
// The test "p starts with base + separator" is wrong for two bases. The
// working directory, ".", is a prefix of no cleaned path (filepath.Join(".",
// "a/b") is "a/b"), so everything below it was refused: p is below "." when it
// is relative and does not climb out ("..", "../x"). A base that ends in a
// separator, the root of a filesystem ("/", `C:\`), has "//" as its prefix, which
// no path starts with: p is below it when it starts with base itself.
func PathWithin(base, p string) bool {
	if p == base {
		return true
	}
	if base == "." {
		if filepath.IsAbs(p) || filepath.VolumeName(p) != "" || p == ".." {
			return false
		}
		return !strings.HasPrefix(p, ".."+string(filepath.Separator))
	}
	if !strings.HasPrefix(p, base) {
		return false
	}
	return base[len(base)-1] == filepath.Separator || p[len(base)] == filepath.Separator
}

// ResolvedWithin is PathWithin for the results of filepath.EvalSymlinks:
// resolved is the resolved path and base the resolved base. EvalSymlinks keeps a
// relative path relative unless a symlink on the way has an absolute target, so
// under a relative base (".", "public") a resolved path can be absolute and
// still inside it. That is the case ResolvedWithin settles, with the base made
// absolute and resolved the same way; a resolved path that stays relative is
// compared as it is (one that climbs, "../x", is refused).
func ResolvedWithin(base, resolved string) bool {
	if PathWithin(base, resolved) {
		return true
	}
	if filepath.IsAbs(base) || !filepath.IsAbs(resolved) {
		return false
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	return PathWithin(abs, resolved)
}
