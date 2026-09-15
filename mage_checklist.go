//go:build mage

package main

import (
	"fmt"
	"os"

	"github.com/goceleris/celeris/internal/release"
)

// ReleaseChecklist prints the markdown body of the version-bump reminder
// that opens when a release milestone is created
// (.github/workflows/release-checklist.yml).
//
// The body itself lives in internal/release so the test suite covers it:
// nothing under a mage build tag is reachable by `go test ./...`, and a
// checklist that drifts from mage CheckRelease would send someone to edit
// the wrong file.
//
// VERSION may be "v1.6.0" or "1.6.0".
func ReleaseChecklist() error {
	v := release.StripV(os.Getenv("VERSION"))
	if v == "" {
		return fmt.Errorf("ReleaseChecklist: set VERSION, e.g. VERSION=v1.6.0 mage ReleaseChecklist")
	}
	if !release.SemverRe.MatchString(v) {
		return fmt.Errorf("ReleaseChecklist: %q is not vX.Y.Z or vX.Y.Z-(alpha|beta|rc).N", v)
	}
	fmt.Print(release.Checklist(v))
	return nil
}
