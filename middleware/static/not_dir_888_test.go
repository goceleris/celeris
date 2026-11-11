package static

import (
	"path/filepath"
	"testing"
)

// A path below a regular file ("app.js/x") failed os.Stat with ENOTDIR, which
// was returned as the handler's error (a 500), where a path below a missing
// name is passed on (a 404): the status told a client which names are files
// (celeris#888 item 91).
func TestPathBelowAFileIsAnsweredAsMissing888(t *testing.T) {
	r := newRoot888(t)
	r.write(t, filepath.Join(r.root, "app.js"), []byte("plain"), secretTime888)
	for _, cfg := range []Config{{Root: r.root}, {Root: r.root, Compress: true}} {
		mw := New(cfg)
		for _, p := range []string{"/app.js/x", "/app.js/x/y"} {
			rec, err := get888(t, mw, p)
			if !refused888(rec, err) {
				t.Errorf("Compress=%v GET %s: status %d err %v, want the request passed on as for a missing name", cfg.Compress, p, rec.StatusCode, err)
			}
		}
	}
}
