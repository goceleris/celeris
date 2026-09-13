//go:build mage

package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// releaseStamp is one place where the module's version is written by hand.
// Every stamp must agree with the release tag; the release workflow refuses
// to create a tag while any of them disagree, and CI refuses to merge a PR
// that leaves them disagreeing with each other.
//
// Why by hand at all: celeris is a library, so consumers build it and no
// ldflags of ours reach them; the only way `celeris.Version` can be right
// is if the source says so. What we can automate is never forgetting.
type releaseStamp struct {
	path string
	// re matches the one line that carries the version, with exactly one
	// capture group holding the version without its leading "v".
	re *regexp.Regexp
	// line renders that line for a given version (without the "v").
	line func(v string) string
}

const semverCore = `(\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?)`

// releaseSubModules are the modules the release workflow tags as
// middleware/<name>/vX.Y.Z; each pins the root module at the same version.
var releaseSubModules = []string{"middleware/compress", "middleware/metrics", "middleware/otel", "middleware/protobuf"}

func releaseStamps() []releaseStamp {
	stamps := []releaseStamp{
		{
			path: "server.go",
			re:   regexp.MustCompile(`(?m)^const Version = "` + semverCore + `"$`),
			line: func(v string) string { return `const Version = "` + v + `"` },
		},
		{
			path: "README.md",
			re:   regexp.MustCompile(`(?m)^## What's new in v` + semverCore + `$`),
			line: func(v string) string { return "## What's new in v" + v },
		},
	}
	for _, sub := range releaseSubModules {
		stamps = append(stamps, releaseStamp{
			path: sub + "/go.mod",
			re:   regexp.MustCompile(`(?m)^\tgithub\.com/goceleris/celeris v` + semverCore + `$`),
			line: func(v string) string { return "\tgithub.com/goceleris/celeris v" + v },
		})
	}
	return stamps
}

// readmePlaceholder is what PrepRelease leaves under a freshly renamed
// "What's new" heading. CheckRelease fails while it is still there, so a
// release cannot ship with last version's prose under this version's title.
const readmePlaceholder = "<!-- TODO(release): describe this release; mage CheckRelease fails while this line remains -->"

// stripV accepts "v1.6.0" or "1.6.0" and returns "1.6.0".
func stripV(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

var semverRe = regexp.MustCompile(`^` + semverCore + `$`)

// stampValue returns the version a stamp currently carries.
func stampValue(s releaseStamp) (string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return "", err
	}
	m := s.re.FindAllStringSubmatch(string(b), -1)
	switch len(m) {
	case 1:
		return m[0][1], nil
	case 0:
		return "", fmt.Errorf("%s: no line matches %s", s.path, s.re)
	default:
		return "", fmt.Errorf("%s: %d lines match %s, want exactly one", s.path, len(m), s.re)
	}
}

// CheckRelease verifies that every version stamp agrees. With VERSION set
// (v1.6.0 or 1.6.0) each stamp must equal it; without, they must all equal
// server.go's Version. CI runs the second form on every PR; the release
// workflow runs the first before it creates a tag. It also fails while
// README.md still carries PrepRelease's placeholder.
func CheckRelease() error {
	want := stripV(os.Getenv("VERSION"))
	stamps := releaseStamps()
	if want == "" {
		v, err := stampValue(stamps[0])
		if err != nil {
			return err
		}
		want = v
	}
	if !semverRe.MatchString(want) {
		return fmt.Errorf("version %q is not vX.Y.Z or vX.Y.Z-(alpha|beta|rc).N", want)
	}
	var bad []string
	for _, s := range stamps {
		got, err := stampValue(s)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		mark := "ok  "
		if got != want {
			mark = "MISMATCH"
			bad = append(bad, fmt.Sprintf("%s carries %s, want %s", s.path, got, want))
		}
		fmt.Printf("  %-8s %-32s %s\n", mark, s.path, got)
	}
	if b, err := os.ReadFile("README.md"); err == nil && strings.Contains(string(b), readmePlaceholder) {
		bad = append(bad, "README.md: the What's new section is still the PrepRelease placeholder; write the release prose")
	}
	if len(bad) > 0 {
		return fmt.Errorf("release stamps disagree with %s:\n  %s\n(run: VERSION=v%s mage PrepRelease)", want, strings.Join(bad, "\n  "), want)
	}
	fmt.Printf("release stamps agree: %s\n", want)
	return nil
}

// PrepRelease sets every version stamp to VERSION (v1.6.0 or 1.6.0): the
// Version constant, the four sub-module pins and the README heading. When
// the README heading moves it leaves a placeholder under the new heading
// that CheckRelease refuses, so the release prose cannot be forgotten
// either. Commit the result through a normal PR, then run the Release
// workflow with the same version; it re-checks everything before tagging.
func PrepRelease() error {
	want := stripV(os.Getenv("VERSION"))
	if !semverRe.MatchString(want) {
		return fmt.Errorf("VERSION=%q: want vX.Y.Z or vX.Y.Z-(alpha|beta|rc).N", os.Getenv("VERSION"))
	}
	for _, s := range releaseStamps() {
		b, err := os.ReadFile(s.path)
		if err != nil {
			return err
		}
		src := string(b)
		loc := s.re.FindAllStringSubmatchIndex(src, -1)
		if len(loc) != 1 {
			return fmt.Errorf("%s: %d lines match %s, want exactly one", s.path, len(loc), s.re)
		}
		had := src[loc[0][2]:loc[0][3]]
		if had == want {
			fmt.Printf("  same     %-32s %s\n", s.path, want)
			continue
		}
		newLine := s.line(want)
		if s.path == "README.md" {
			newLine += "\n\n" + readmePlaceholder
		}
		src = src[:loc[0][0]] + newLine + src[loc[0][1]:]
		if err := os.WriteFile(s.path, []byte(src), 0o644); err != nil {
			return err
		}
		fmt.Printf("  %s -> %s  %s\n", had, want, s.path)
	}
	fmt.Println("stamps set; README.md needs the release prose (CheckRelease will say so until it has it)")
	return nil
}
