package ctxkit

import (
	"path/filepath"
)

// FilesystemRoot reports whether root, a cleaned path (relative or absolute), resolves to
// the root of a filesystem: "/", a drive root such as C:\, a UNC share root. The
// root is made absolute against the working directory, and its symlinks are
// resolved, so "." in a working directory of "/", "../../.." reaching "/" and a
// symlink to "/" are found; a root that does not exist, or cannot be resolved, is
// not one (it serves nothing). It returns the resolved path. The working directory
// is read now: a later chdir is not covered.
func FilesystemRoot(root string) (string, bool) {
	abs, err := absPhysical(filepath.Clean(root))
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	resolved = filepath.Clean(resolved)
	vol := filepath.VolumeName(resolved)
	return resolved, isRootOf(vol, resolved[len(vol):], filepath.Separator)
}

// isRootOf reports whether a cleaned path, split into its volume name and the
// rest, is the root of that volume: the rest is one separator, or empty for a
// UNC share (\\host\share), which is a root without one. A path with no volume
// and an empty rest is no path at all.
func isRootOf(vol, rest string, sep byte) bool {
	if len(rest) == 1 && rest[0] == sep {
		return true
	}
	return rest == "" && len(vol) > 2 && vol[0] == sep && vol[1] == sep
}
