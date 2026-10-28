package ctxkit

import (
	"os"
	"path/filepath"
	"strings"
)

// PathWithin reports whether p is base or a path below it. Both are cleaned
// paths (filepath.Clean), of the same kind: both relative or both absolute.
//
// The test "p starts with base + separator" is wrong for three kinds of base.
// The working directory, ".", is a prefix of no cleaned path (filepath.Join(".",
// "a/b") is "a/b"), so everything below it was refused: p is below "." when it
// is relative and does not climb out ("..", "../x"). A base that ends in a
// separator, the root of a filesystem ("/", `C:\`), has "//" as its prefix, which
// no path starts with: p is below it when it starts with base itself. And a
// relative base that starts with ".." names a directory above the working
// directory, so a path that "starts with" it says nothing ("../../etc/passwd"
// starts with ".." and a separator, and is not below ".."): such a base and p are
// made absolute, against the working directory with its symlinks resolved, and
// compared as absolute paths. A Root of "..", "../.." or "../x/.." serves the
// directory it resolves to and nothing outside it.
func PathWithin(base, p string) bool {
	if p == base {
		return true
	}
	if !filepath.IsAbs(base) && climbs(base) {
		ab, err := absPhysical(base)
		if err != nil {
			return false
		}
		ap, err := absPhysical(p)
		if err != nil {
			return false
		}
		return withinClean(ab, ap)
	}
	return withinClean(base, p)
}

// climbs reports whether the cleaned relative path starts with "..".
func climbs(p string) bool {
	return p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator))
}

// withinClean is PathWithin for two cleaned paths whose base does not climb.
func withinClean(base, p string) bool {
	if p == base {
		return true
	}
	if base == "." {
		if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
			return false
		}
		return !climbs(p)
	}
	if !strings.HasPrefix(p, base) {
		return false
	}
	return base[len(base)-1] == filepath.Separator || p[len(base)] == filepath.Separator
}

// absPhysical makes the cleaned path absolute against the working directory
// with its symlinks resolved, so that ".." in the result climbs the way the
// operating system does (a working directory reached through a symlink has a
// different parent than its logical path has). An absolute path is returned
// as it is.
func absPhysical(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	wd, err = filepath.EvalSymlinks(wd)
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// ResolvedWithin is PathWithin for the results of filepath.EvalSymlinks:
// resolved is the resolved path and base the resolved base. EvalSymlinks keeps a
// relative path relative unless a symlink on the way has an absolute target, so
// under a relative base (".", "public") a resolved path can be absolute and
// still inside it, or relative and climb through the base's own name
// ("../c/a" from a working directory c) and still be inside. When the lexical
// comparison refuses and one of the two is relative, both are made absolute
// (absPhysical; neither holds a symlink any more, so the result is exact) and
// compared; two absolute paths are compared as they are.
func ResolvedWithin(base, resolved string) bool {
	if PathWithin(base, resolved) {
		return true
	}
	if filepath.IsAbs(base) && filepath.IsAbs(resolved) {
		return false
	}
	ab, err := absPhysical(base)
	if err != nil {
		return false
	}
	ar, err := absPhysical(resolved)
	if err != nil {
		return false
	}
	return withinClean(ab, ar)
}

// ResolveClimbingRoot returns root, a cleaned path, made absolute against the
// working directory (symlinks resolved) when it is relative and starts with
// ".." ("..", "../..", "../x"): such a root names a directory above the working
// directory, and each containment check would otherwise have to resolve the
// working directory again (a Getwd and an EvalSymlinks per call). Done once,
// when the middleware or the route is made, a later change of the working
// directory does not move the root. Any other root, and one that cannot be
// resolved, is returned as it is.
func ResolveClimbingRoot(root string) string {
	if root == "" || filepath.IsAbs(root) || !climbs(root) {
		return root
	}
	abs, err := absPhysical(root)
	if err != nil {
		return root
	}
	return abs
}
