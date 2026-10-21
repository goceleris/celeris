package release

// RehearsalVersion returns the version .github/scripts/pinmove-rehearsal.sh
// rehearses when it is given none. This is the stub the failing-first test
// runs against; the real rule follows in the next commit.
func RehearsalVersion(current string, tags []string) (string, error) {
	return "", nil
}
