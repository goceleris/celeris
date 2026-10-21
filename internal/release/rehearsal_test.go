package release

import "testing"

// The rehearsal must move the pins FORWARD from the last release, before and
// after the release it prepares ships. A hardcoded default stops doing that the
// day the release is tagged (it would rehearse a downgrade), so the default is
// derived from the Version constant and the release tags.
func TestRehearsalVersion(t *testing.T) {
	cases := []struct {
		name    string
		current string
		tags    []string
		want    string
	}{
		{"before v1.6.0 ships", "1.6.0", []string{"v1.5.10", "v1.5.11"}, "v1.6.0-rc.99"},
		{"after v1.6.0 ships", "1.6.0", []string{"v1.5.11", "v1.6.0"}, "v1.6.1-rc.99"},
		{"main already names the next minor", "1.7.0", []string{"v1.6.0", "v1.6.3"}, "v1.7.0-rc.99"},
		{"a pre-release Version counts as its core", "1.7.0-rc.1", []string{"v1.6.3"}, "v1.7.0-rc.99"},
		{"Version behind the last release", "1.6.0", []string{"v1.6.0", "v1.6.4"}, "v1.6.5-rc.99"},
		{"numeric, not lexical, order", "1.9.0", []string{"v1.9.0", "v1.10.0", "v1.2.0"}, "v1.10.1-rc.99"},
		{"pre-release and sub-module tags are not releases", "1.6.0",
			[]string{"v1.5.11", "v1.6.0-rc.1", "v1.7.0-rc.2", "middleware/otel/v1.9.9", "not-a-tag"}, "v1.6.0-rc.99"},
	}
	for _, c := range cases {
		got, err := RehearsalVersion(c.current, c.tags)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: RehearsalVersion(%q, %v) = %q, want %q", c.name, c.current, c.tags, got, c.want)
		}
		if !SemverRe.MatchString(StripV(got)) {
			t.Errorf("%s: %q is a version the release gate would refuse", c.name, got)
		}
	}
}

func TestRehearsalVersionRefusesWhatItCannotDerive(t *testing.T) {
	for _, c := range []struct {
		name    string
		current string
		tags    []string
	}{
		{"no release tag at all (a shallow clone)", "1.6.0", nil},
		{"only pre-release tags", "1.6.0", []string{"v1.6.0-rc.1"}},
		{"Version is not a version", "next", []string{"v1.5.11"}},
		{"Version is empty", "", []string{"v1.5.11"}},
	} {
		if got, err := RehearsalVersion(c.current, c.tags); err == nil {
			t.Errorf("%s: got %q, want an error", c.name, got)
		}
	}
}
