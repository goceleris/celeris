package ctxkit

import (
	"io/fs"
	"path"
	"strings"
)

const (
	// maxFSLinkFollows is how many symlinks ResolveFSLinks follows for one name.
	// The kernel allows 40; a served tree has none or one or two (a deploy's
	// "current"), and each follow can cost a walk.
	maxFSLinkFollows = 8
	// maxFSWalkSteps bounds the segments one walk looks at, links included: a
	// planted link that walks many segments per follow ("d/../d/../...") is cut
	// off.
	maxFSWalkSteps = 256
)

// ResolveFSLinks returns name, a slash path below the root of an FS, with every
// symlink on it replaced by its target, as the operating system walks it: the
// place the file is at, for the hidden check and for the pre-open stat. ok is
// false when the walk cannot vouch for the result: a link leads out of the FS (an
// absolute target, or a target that climbs above the FS root, which an
// os.DirFS or an fs.Sub of one still lets the OS follow), more than
// maxFSLinkFollows links are on the way (a loop included), or the walk takes more
// than maxFSWalkSteps steps. Callers that refuse hidden files refuse such a
// path as well: the FS cannot say what is beyond it.
//
// Without any link on the way (nearly every request) the walk allocates no
// path: it looks at the prefixes of name, one Lstat each.
func ResolveFSLinks(rl fs.ReadLinkFS, name string) (resolved string, ok bool) {
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "./") {
		// an FS name is not cleaned for the caller: clean as the OS would
		name = path.Clean(name)
		if name == ".." || strings.HasPrefix(name, "../") {
			return name, true // not a valid FS name: it cannot be opened, and finds nothing
		}
	}
	for end := 0; end < len(name); {
		i := strings.IndexByte(name[end:], '/')
		if i < 0 {
			end = len(name)
		} else {
			end += i
		}
		fi, err := rl.Lstat(name[:end])
		if err != nil {
			return name, true // it does not exist: no link beyond that point
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return walkFSLinks(rl, name)
		}
		end++
	}
	return name, true
}

// walkFSLinks is the general walk, for a name with a link on it.
func walkFSLinks(rl fs.ReadLinkFS, name string) (string, bool) {
	queue := strings.Split(name, "/")
	var resolved []string
	follows := 0
	for steps := 0; len(queue) > 0; steps++ {
		if steps > maxFSWalkSteps {
			return "", false
		}
		seg := queue[0]
		queue = queue[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", false // above the FS root: the FS cannot say what is there
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		cand := strings.Join(append(resolved, seg), "/")
		fi, err := rl.Lstat(cand)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			resolved = append(resolved, seg)
			continue
		}
		if follows++; follows > maxFSLinkFollows {
			return "", false
		}
		target, err := rl.ReadLink(cand)
		if err != nil {
			return "", false
		}
		target = strings.ReplaceAll(target, `\`, "/")
		if strings.HasPrefix(target, "/") || (len(target) > 1 && target[1] == ':') {
			return "", false // an absolute target leaves the FS
		}
		queue = append(strings.Split(target, "/"), queue...)
	}
	return strings.Join(resolved, "/"), true
}
