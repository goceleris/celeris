package celeris

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// serveOpenFile checks the open descriptor again, not only the stat that came
// before it: a path that was a regular file when it was stat-ed and is something
// else when it is opened (a swap needs write access to the tree) is refused,
// whatever the path says. The test puts a device under the path after the stat.
func TestServeOpenFileChecksTheOpenDescriptor888(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/null")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reg.txt"), []byte("REG"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(os.DevNull); err != nil {
		t.Skip(err)
	}
	old := openForServe
	t.Cleanup(func() { openForServe = old })
	openForServe = func(string) (*os.File, error) { return os.Open(os.DevNull) }
	status, body, err := fileFromDir888(t, dir, "reg.txt")
	if !isNotRegular888(err) || status == 200 || body != "" {
		t.Errorf("FileFromDir with a device under the path: status %d body %q err %v, want an HTTPError 404 (not a regular file)", status, body, err)
	}
}
