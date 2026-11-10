package ctxkit

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// linkOnly is an FS that reads links but has no Stat, as an fs.Sub of an os.DirFS.
type linkOnly struct{ m fstest.MapFS }

func (l linkOnly) Open(name string) (fs.File, error)      { return l.m.Open(name) }
func (l linkOnly) ReadLink(name string) (string, error)   { return l.m.ReadLink(name) }
func (l linkOnly) Lstat(name string) (fs.FileInfo, error) { return l.m.Lstat(name) }

func TestNotRegularFS(t *testing.T) {
	m := fstest.MapFS{
		"reg":        {Data: []byte("x"), Mode: 0o644},
		"fifo":       {Mode: fs.ModeNamedPipe | 0o644},
		"dev":        {Mode: fs.ModeDevice | fs.ModeCharDevice},
		"d/file":     {Data: []byte("y")},
		"lnk-fifo":   {Data: []byte("fifo"), Mode: fs.ModeSymlink},
		"lnk-reg":    {Data: []byte("reg"), Mode: fs.ModeSymlink},
		"lnk-out":    {Data: []byte("../outside"), Mode: fs.ModeSymlink},
		"lnk-abs":    {Data: []byte("/etc/x"), Mode: fs.ModeSymlink},
		"d2/in-fifo": {Data: []byte("../fifo"), Mode: fs.ModeSymlink},
	}
	for _, tc := range []struct {
		name                 string
		wantBad, wantChecked bool
	}{
		{"reg", false, true}, {"fifo", true, true}, {"dev", true, true}, {"d", false, true}, {"d/file", false, true},
		{"missing", false, true}, {"lnk-fifo", true, true}, {"lnk-reg", false, true}, {"d2/in-fifo", true, true},
	} {
		// a StatFS follows the links itself (a MapFS does not follow links in Stat, so
		// it is only asked about the names that are not links)
		if tc.name == "lnk-fifo" || tc.name == "lnk-reg" || tc.name == "d2/in-fifo" {
			continue
		}
		if bad, checked := NotRegularFS(m, tc.name); bad != tc.wantBad || checked != tc.wantChecked {
			t.Errorf("StatFS: NotRegularFS(%q) = %v, %v; want %v, %v", tc.name, bad, checked, tc.wantBad, tc.wantChecked)
		}
	}
	lo := linkOnly{m}
	for _, tc := range []struct {
		name                 string
		wantBad, wantChecked bool
	}{
		{"reg", false, true}, {"fifo", true, true}, {"dev", true, true}, {"d", false, true}, {"missing", false, true},
		{"lnk-fifo", true, true}, {"lnk-reg", false, true}, {"d2/in-fifo", true, true},
		{"lnk-out", false, false}, {"lnk-abs", false, false},
	} {
		if bad, checked := NotRegularFS(lo, tc.name); bad != tc.wantBad || checked != tc.wantChecked {
			t.Errorf("link-only FS: NotRegularFS(%q) = %v, %v; want %v, %v", tc.name, bad, checked, tc.wantBad, tc.wantChecked)
		}
	}
	// an FS with neither: nothing to look with
	if bad, checked := NotRegularFS(onlyOpen{m}, "fifo"); bad || checked {
		t.Errorf("open-only FS: NotRegularFS = %v, %v; want false, false", bad, checked)
	}
}

type onlyOpen struct{ m fstest.MapFS }

func (o onlyOpen) Open(name string) (fs.File, error) { return o.m.Open(name) }
