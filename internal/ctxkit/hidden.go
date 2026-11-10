package ctxkit

import (
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrHidden is returned by the registered FileFromDir when the path it was
// asked to serve, once its symlinks are resolved, has a hidden segment (see
// HiddenPath) and the caller asked for hidden paths to be refused. Nothing has
// been written to the response.
var ErrHidden = errors.New("hidden path")

// ErrNotRegular is carried (wrapped in an HTTPError 404) by the error of the
// registered FileFromDir, and by Context.File and FileFromFS, for a path that is
// not a regular file once symlinks are resolved: a device, a FIFO, a socket.
// Nothing has been written to the response.
var ErrNotRegular = errors.New("not a regular file")

// windows is whether a backslash separates path elements.
var windows = runtime.GOOS == "windows"

// wellKnown is the one dot segment that is not hidden: RFC 8615 (ACME
// challenges, security.txt, apple-app-site-association). Only this exact name.
const wellKnown = ".well-known"

// HiddenSegment reports whether one path segment names a hidden file or
// directory: it starts with a dot (".env", ".git", ".ssh", ".htaccess",
// ".", ".."), except the exact name ".well-known". A literal "." segment is
// not a name and never reaches it (HiddenPath drops it first); "%2e" does, and
// is hidden. A segment that percent-decodes to such a name is hidden too, so a layer that
// decodes the path later cannot turn a visible segment into a dotfile.
func HiddenSegment(seg string) bool {
	if hiddenDecoded(seg) {
		return true
	}
	if !windows && strings.IndexByte(seg, '\\') >= 0 {
		// A backslash is a name character here, not a separator: the file system
		// sees one name. But a piece of it that starts with a dot makes it hidden
		// all the same ("a\.b"), and ".." is hidden too, so it can never pop a
		// segment before it.
		for _, piece := range strings.Split(seg, "\\") {
			if hiddenDecoded(piece) {
				return true
			}
		}
	}
	return false
}

func hiddenDecoded(seg string) bool {
	if hiddenName(seg) {
		return true
	}
	if strings.IndexByte(seg, '%') >= 0 {
		if dec, err := url.PathUnescape(seg); err == nil && dec != seg && hiddenName(dec) {
			return true
		}
	}
	return false
}

func hiddenName(s string) bool {
	return len(s) > 0 && s[0] == '.' && s != wellKnown
}

// separators is what separates the segments of a path for HiddenPath: "/", and
// the backslash where it is a separator too (Windows).
func separators() string {
	if windows {
		return `/\`
	}
	return "/"
}

// HiddenPath reports whether a slash path below a root has a hidden segment
// (HiddenSegment). The path is cleaned lexically first, as filepath.Join cleans
// the path that is then opened: empty and "." segments are dropped and ".."
// removes the segment before it, so "x/../.env" is ".env" and "/.git/../a" is
// "a". A backslash separates segments only where it is a separator (Windows);
// elsewhere it is part of a name, and a segment that holds one is hidden if any
// piece of it starts with a dot (HiddenSegment): "/.git/..\x" is the hidden
// ".git", and a file named "..\x" is never taken for "x". Only the segments of p
// count: the name of the root the path is below never does.
func HiddenPath(p string) bool {
	var stack [16]string
	segs := stack[:0]
	for len(p) > 0 {
		i := strings.IndexAny(p, separators())
		var seg string
		if i < 0 {
			seg, p = p, ""
		} else {
			seg, p = p[:i], p[i+1:]
		}
		switch seg {
		case "", ".":
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	for _, s := range segs {
		if HiddenSegment(s) {
			return true
		}
	}
	return false
}

// RelWithin returns the slash path of p below base, for a p that
// ResolvedWithin(base, p) accepted. Both are results of filepath.EvalSymlinks.
func RelWithin(base, p string) (string, bool) {
	if (filepath.IsAbs(base) || !climbs(base)) && withinClean(base, p) {
		switch {
		case p == base:
			return "", true
		case base == ".":
			return filepath.ToSlash(p), true
		case base[len(base)-1] == filepath.Separator:
			return filepath.ToSlash(p[len(base):]), true
		default:
			return filepath.ToSlash(p[len(base)+1:]), true
		}
	}
	ab, err := absPhysical(base)
	if err != nil {
		return "", false
	}
	ap, err := absPhysical(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(ab, ap)
	if err != nil || climbs(rel) {
		return "", false
	}
	if rel == "." {
		rel = ""
	}
	return filepath.ToSlash(rel), true
}
