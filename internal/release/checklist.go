// Package release holds the facts about where this module's version is
// written down, and renders the reminder that opens when a release
// milestone is created.
//
// It lives in a normal package rather than beside the mage targets for one
// reason: the mage files are `package main` behind a build tag, so nothing
// in them is reachable by `go test ./...` and none of it is covered by CI.
// A checklist that drifts from the checker is worse than no checklist, so
// the part that must not drift lives where the test suite can see it.
package release

import (
	"fmt"
	"regexp"
	"strings"
)

// SemverCore matches a version without its leading "v", with one capture
// group. Shared so the stamp patterns and the validator cannot disagree.
const SemverCore = `(\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?)`

// SemverRe matches a whole version string, with or without the leading "v".
var SemverRe = regexp.MustCompile(`^` + SemverCore + `$`)

// SubModules are the modules a release tags as middleware/<name>/vX.Y.Z.
// Each pins the root module at the same version.
var SubModules = []string{
	"middleware/compress",
	"middleware/metrics",
	"middleware/otel",
	"middleware/protobuf",
}

// Stamp is one place the version is written by hand.
//
// Why by hand at all: celeris is a library, so consumers build it themselves
// and no ldflags of ours reach them. The only way celeris.Version can be
// right is if the source says so. What can be automated is never forgetting.
type Stamp struct {
	Path string
	// Re matches the one line carrying the version, with exactly one
	// capture group holding it without the leading "v".
	Re *regexp.Regexp
	// Line renders that line for a given version (without the "v").
	Line func(v string) string
}

// Stamps returns every place the version must be updated, in the order a
// person would edit them.
func Stamps() []Stamp {
	out := make([]Stamp, 0, 2+len(SubModules))
	out = append(out, []Stamp{
		{
			Path: "server.go",
			Re:   regexp.MustCompile(`(?m)^const Version = "` + SemverCore + `"$`),
			Line: func(v string) string { return `const Version = "` + v + `"` },
		},
		{
			Path: "README.md",
			Re:   regexp.MustCompile(`(?m)^## What's new in v` + SemverCore + `$`),
			Line: func(v string) string { return "## What's new in v" + v },
		},
	}...)
	for _, sub := range SubModules {
		out = append(out, Stamp{
			Path: sub + "/go.mod",
			Re:   regexp.MustCompile(`(?m)^\tgithub\.com/goceleris/celeris v` + SemverCore + `$`),
			Line: func(v string) string { return "\tgithub.com/goceleris/celeris v" + v },
		})
	}
	return out
}

// StripV accepts "v1.6.0" or "1.6.0" and returns "1.6.0".
func StripV(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Checklist renders the markdown body of the version-bump reminder for v
// (given without its leading "v").
//
// It is generated from Stamps and SubModules rather than written out, so
// adding a stamp adds a line here and TestChecklistCoversEveryStamp fails if
// the two ever disagree.
func Checklist(v string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Opened automatically because the **v%s** milestone was created. "+
		"Close it when every stamp below reads `%s`.\n\n", v, v)

	b.WriteString("Only the celeris stamps are enforced: `mage CheckRelease` runs in Lint on " +
		"every pull request and fails while any of them disagrees. Everything under the second " +
		"heading is enforced by nothing, which is why it is written down here.\n\n")

	b.WriteString("## celeris — enforced by `mage CheckRelease`\n\n")
	b.WriteString("One command does all of these:\n\n")
	fmt.Fprintf(&b, "```bash\nVERSION=v%s mage PrepRelease\n```\n\n", v)
	b.WriteString("Or by hand:\n\n")
	for _, s := range Stamps() {
		fmt.Fprintf(&b, "- [ ] `%s` → `%s`\n", s.Path, strings.TrimLeft(s.Line(v), "\t"))
	}
	b.WriteString("- [ ] `README.md` — replace the placeholder under that heading with the " +
		"real release prose. `CheckRelease` fails while the placeholder remains, so this one " +
		"cannot be forgotten silently.\n\n")

	b.WriteString("## The other three repositories — enforced by nothing\n\n")
	b.WriteString("- [ ] **loadgen** `version.go`, `const fallbackVersion` — only if loadgen " +
		"is being released too; it versions independently of celeris.\n")
	b.WriteString("- [ ] **loadgen** `internal/integrationtest/testserver/go.mod` — the celeris pin.\n")
	fmt.Fprintf(&b, "- [ ] **probatorium** — repin celeris in all ten modules that require it, "+
		"to the released tag rather than a pseudo-version:\n"+
		"  ```bash\n"+
		"  for m in $(grep -rl 'goceleris/celeris v' --include=go.mod . | grep -v .claude); do\n"+
		"    (cd \"$(dirname \"$m\")\" && go get github.com/goceleris/celeris@v%s && go mod tidy)\n"+
		"  done\n"+
		"  ```\n", v)
	b.WriteString("- [ ] **docs** — publish this version's benchmark results, so the dashboard " +
		"is not left a release behind. It has been, for three releases running.\n\n")

	b.WriteString("## After you publish the release\n\n")
	b.WriteString("These run on their own from `.github/workflows/release.yml`. " +
		"They are listed so a failure is recognisable rather than invisible:\n\n")
	for _, sub := range SubModules {
		fmt.Fprintf(&b, "- `%s/v%s` is tagged\n", sub, v)
	}
	b.WriteString("- the Go module proxy is asked for the root module and every sub-module, " +
		"then re-queried to confirm each is really available\n\n")

	b.WriteString("Neither runs if the stamps disagree. A sub-module tag pinning the wrong " +
		"celeris version cannot be corrected afterwards: every published tag is recorded in " +
		"`sum.golang.org`, and moving one gives existing consumers a checksum mismatch that " +
		"reads as a supply-chain compromise. The remedy for a bad published version is always " +
		"a new version.\n")

	return b.String()
}
