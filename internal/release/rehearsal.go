package release

import (
	"fmt"
	"regexp"
	"strconv"
)

var (
	releaseTagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	coreRe       = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-(?:alpha|beta|rc)\.\d+)?$`)
)

type triple [3]int

func (a triple) greater(b triple) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parseTriple(m []string) triple {
	var t triple
	for i := range t {
		t[i], _ = strconv.Atoi(m[i+1]) // the regexps only let digits through
	}
	return t
}

// RehearsalVersion returns the version .github/scripts/pinmove-rehearsal.sh
// rehearses when it is given none.
//
// current is the Version constant of server.go (without the "v"); tags are
// the repository's tags. Only vX.Y.Z tags count as releases: pre-release tags
// and sub-module tags (middleware/otel/v1.6.0) do not.
//
// The rehearsal must move the sub-module pins forward from the last release,
// so the answer is always newer than the last release:
//
//   - the numeric core of current, when it is newer than the last release
//     (before v1.6.0 ships: 1.6.0 with last release v1.5.11 gives v1.6.0-rc.99)
//   - otherwise the next patch of the last release (after v1.6.0 ships:
//     v1.6.1-rc.99), so a run after the release does not rehearse a downgrade.
//
// The -rc.99 suffix keeps it a name no real release will ever take, and one
// the release gate accepts (SemverRe).
func RehearsalVersion(current string, tags []string) (string, error) {
	cm := coreRe.FindStringSubmatch(current)
	if cm == nil {
		return "", fmt.Errorf("server.go Version %q is not X.Y.Z or X.Y.Z-(alpha|beta|rc).N", current)
	}
	core := parseTriple(cm)

	var last triple
	found := false
	for _, tag := range tags {
		m := releaseTagRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		if t := parseTriple(m); !found || t.greater(last) {
			last, found = t, true
		}
	}
	if !found {
		return "", fmt.Errorf("no vX.Y.Z release tag to move the pins from (a shallow clone? fetch with full history and tags)")
	}

	next := core
	if !core.greater(last) {
		next = triple{last[0], last[1], last[2] + 1}
	}
	return fmt.Sprintf("v%d.%d.%d-rc.99", next[0], next[1], next[2]), nil
}
