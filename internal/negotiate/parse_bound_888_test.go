package negotiate

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// ParseInto reads at most 64 entries of a header (celeris#888 item 42). Accept
// runs it on the client's whole header, and before the bound an
// Accept-Encoding of "a,a,a,..." cost about 60 to 70 times its length in heap
// (3.8 MB for the 64 KiB the native engines accept, 1.2 GB for the 16 MiB the
// std engine's default MaxHeaderBytes lets through).

const wantMaxEntries888 = 64

func TestParseIntoBoundsTheEntryCount888(t *testing.T) {
	for _, n := range []int{wantMaxEntries888, wantMaxEntries888 + 1, 1000, 32768} {
		h := strings.Repeat("a,", n)
		got := Parse(h)
		want := min(n, wantMaxEntries888)
		if len(got) != want {
			t.Errorf("%d entries: Parse returned %d, want %d", n, len(got), want)
		}
	}
}

// The entries kept are the first ones, in order.
func TestParseIntoKeepsTheFirstEntries888(t *testing.T) {
	var b strings.Builder
	for i := range 3 * wantMaxEntries888 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "t%d;q=0.5", i)
	}
	got := Parse(b.String())
	if len(got) != wantMaxEntries888 {
		t.Fatalf("Parse returned %d entries, want %d", len(got), wantMaxEntries888)
	}
	for i, e := range got {
		if want := fmt.Sprintf("t%d", i); e.MediaType != want || e.Quality != 0.5 {
			t.Fatalf("entry %d = %+v, want %s;q=0.5", i, e, want)
		}
	}
}

// Empty list elements do not count as entries, but the walk over them is
// bounded as well: a header of commas allocates nothing and ends early.
func TestParseIntoEmptyElementsAreBounded888(t *testing.T) {
	h := strings.Repeat(",", 1<<20) + "gzip"
	if got := Parse(h); len(got) != 0 {
		t.Errorf("a megabyte of commas before gzip: Parse returned %v, want no entries (the walk is bounded)", got)
	}
	// A realistic header with a few empty elements still parses whole.
	if got := Parse("gzip,, ,deflate"); len(got) != 2 {
		t.Errorf("Parse(%q) = %v, want 2 entries", "gzip,, ,deflate", got)
	}
}

// Accept's heap for a long header is bounded by the entry cap, not by the
// header's length. TotalAlloc is read around the calls; the bound is far above
// what the capped parse allocates (a few KB) and far below main's 3.8 MB.
func TestAcceptHeapDoesNotGrowWithTheHeader888(t *testing.T) {
	h := strings.Repeat("a,", 32<<10) // 64 KiB
	offers := []string{"gzip", "br"}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const runs = 20
	for range runs {
		_ = Accept(h, offers)
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 16<<10 {
		t.Errorf("Accept over a 64 KiB header allocated %d bytes per call, want a small bound (main: about 3.8 MB)", per)
	}
}

// What the bound costs, stated: an entry past the 64th is not seen. A coding
// listed after 64 others is not accepted, and a refusal (q=0) past them is
// not applied. Both need a header of more than 64 list items, which no browser
// sends.
func TestEntriesPastTheBoundAreNotSeen888(t *testing.T) {
	pad := strings.Repeat("x,", wantMaxEntries888)
	if got := Accept(pad+"gzip", []string{"gzip"}); got != "" {
		t.Errorf("gzip listed after %d other entries: Accept = %q, want %q (not seen)", wantMaxEntries888, got, "")
	}
	if got := Accept("*,"+strings.Repeat("x,", wantMaxEntries888)+"gzip;q=0", []string{"gzip"}); got != "gzip" {
		t.Errorf("gzip;q=0 listed after %d other entries: Accept = %q, want gzip (the refusal is not seen)", wantMaxEntries888, got)
	}
	// Within the bound everything is as before.
	if got := Accept(strings.Repeat("x,", wantMaxEntries888-2)+"gzip", []string{"gzip"}); got != "gzip" {
		t.Errorf("gzip as the %dth entry: Accept = %q, want gzip", wantMaxEntries888-1, got)
	}
	if got := Accept("*,"+strings.Repeat("x,", wantMaxEntries888-3)+"gzip;q=0", []string{"gzip"}); got != "" {
		t.Errorf("gzip;q=0 as the %dth entry: Accept = %q, want it refused", wantMaxEntries888-1, got)
	}
}
