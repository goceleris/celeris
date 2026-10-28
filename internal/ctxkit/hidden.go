package ctxkit

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

// ErrHidden is returned by the registered FileFromDir when the path it was
// asked to serve, once its symlinks are resolved, has a hidden segment (see
// HiddenPath) and the caller asked for hidden paths to be refused. Nothing has
// been written to the response.
var ErrHidden = errors.New("hidden path")

// wellKnown is the one dot segment that is not hidden: RFC 8615 (ACME
// challenges, security.txt, apple-app-site-association). Only this exact name.
const wellKnown = ".well-known"

// HiddenSegment reports whether one path segment names a hidden file or
// directory: it starts with a dot (".env", ".git", ".ssh", ".htaccess",
// ".."), except the exact name ".well-known". The segment "." and the empty
// segment are not names and are not hidden (HiddenPath drops them first). A
// segment that percent-decodes to such a name is hidden too, so a layer that
// decodes the path later cannot turn a visible segment into a dotfile.
func HiddenSegment(seg string) bool {
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

// HiddenPath reports whether a slash path below a root has a hidden segment
// (HiddenSegment). The path is cleaned lexically first, as filepath.Join cleans
// the path that is then opened: empty and "." segments are dropped and ".."
// removes the segment before it, so "x/../.env" is ".env" and "/.git/../a" is
// "a". Backslashes separate segments too (on Windows they do), which makes a
// name that holds a backslash and then a dot ("a\.b") hidden on the other
// systems: harmless. Only the segments of p count: the name of the root the path
// is below never does.
func HiddenPath(p string) bool {
	var stack [16]string
	segs := stack[:0]
	for len(p) > 0 {
		i := strings.IndexAny(p, `/\`)
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
	if !(!filepath.IsAbs(base) && climbs(base)) && withinClean(base, p) {
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
