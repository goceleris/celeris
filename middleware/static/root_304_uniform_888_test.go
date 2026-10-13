package static

import (
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A conditional Root request for a path that leaves the root is refused (400)
// whatever its validator says; the check that finds out costs a few syscalls
// (resolvesInside), so if it ran only when the validator matched the target's
// stat, the response time would tell whether a guessed mtime and size are right
// (celeris#888 item 35, the same oracle the 304 was). A timing assertion is
// flaky; the pinned property is that the check runs once for every
// conditional request, a matching guess or not, If-None-Match or
// If-Modified-Since.
func TestRoot304ContainmentRunsForEveryGuess888(t *testing.T) {
	var calls atomic.Int32
	resolvesInsideHook = func() { calls.Add(1) }
	t.Cleanup(func() { resolvesInsideHook = nil })

	tag := weakETag(secretTime888, int64(len(secretBytes888)))
	wrongTag := weakETag(secretTime888.Add(time.Second), int64(len(secretBytes888)))
	date := secretTime888.UTC().Format(http.TimeFormat)
	earlier := secretTime888.Add(-time.Hour).UTC().Format(http.TimeFormat)
	guesses := []struct {
		name string
		hdr  []string
	}{
		{"if-none-match-right", []string{"if-none-match", tag}},
		{"if-none-match-wrong", []string{"if-none-match", wrongTag}},
		{"if-none-match-star", []string{"if-none-match", "*"}},
		{"if-modified-since-right", []string{"if-modified-since", date}},
		{"if-modified-since-wrong", []string{"if-modified-since", earlier}},
		{"if-modified-since-unparsable", []string{"if-modified-since", "not a date"}},
	}
	targets := []struct {
		name  string
		setup func(t *testing.T, r root888)
		path  string
		want  int // status of the unguarded outcome; 0 with a 400 error
	}{
		{"escaping-leaf", func(t *testing.T, r root888) {
			r.link(t, filepath.Join(r.outside, "secret.txt"), filepath.Join("a", "b", "leak.txt"))
		}, "/a/b/leak.txt", 400},
		{"escaping-parent-directory", func(t *testing.T, r root888) {
			r.link(t, r.outside, "linkdir")
		}, "/linkdir/secret.txt", 400},
		{"file-inside", func(t *testing.T, r root888) {
			r.write(t, filepath.Join(r.root, "d", "f.txt"), secretBytes888, secretTime888)
		}, "/d/f.txt", 0},
	}
	for _, tg := range targets {
		for _, g := range guesses {
			t.Run(tg.name+"/"+g.name, func(t *testing.T) {
				r := newRoot888(t)
				tg.setup(t, r)
				mw := New(Config{Root: r.root})
				calls.Store(0)
				rec, err := get888(t, mw, tg.path, g.hdr...)
				if tg.want == 400 {
					if rec.StatusCode == 304 || !isHTTP400(err) {
						t.Errorf("status %d, err %v; want a 400", rec.StatusCode, err)
					}
				} else if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if n := calls.Load(); n != 1 {
					t.Errorf("the containment check ran %d times, want exactly once for every guess", n)
				}
			})
		}
	}
}
