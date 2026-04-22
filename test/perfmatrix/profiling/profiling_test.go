package profiling

import (
	"context"
	"net"
	"net/http"
	_ "net/http/pprof" // registers pprof handlers on http.DefaultServeMux
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newPprofServer starts a localhost http server serving the default
// ServeMux (which has /debug/pprof/* registered via the blank import).
// Returns the base URL and a shutdown function.
func newPprofServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.DefaultServeMux}
	go func() { _ = srv.Serve(ln) }()
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return "http://" + ln.Addr().String(), shutdown
}

func TestCaptureCPUWritesNonEmptyFile(t *testing.T) {
	base, shutdown := newPprofServer(t)
	t.Cleanup(shutdown)

	dir := t.TempDir()
	cfg := Config{
		TargetURL: base,
		OutDir:    dir,
		CPUDur:    1 * time.Second, // short window so the test stays fast
	}
	path, err := CaptureCPU(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CaptureCPU: %v", err)
	}
	if path == "" {
		t.Fatalf("CaptureCPU returned empty path")
	}
	if filepath.Dir(path) != dir {
		t.Errorf("CaptureCPU wrote outside OutDir: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Errorf("CPU profile is empty")
	}
}

func TestCaptureHeapWritesNonEmptyFile(t *testing.T) {
	base, shutdown := newPprofServer(t)
	t.Cleanup(shutdown)

	dir := t.TempDir()
	cfg := Config{TargetURL: base, OutDir: dir}
	path, err := CaptureHeap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CaptureHeap: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("heap profile is empty")
	}
}

func TestCaptureGoroutineWritesNonEmptyFile(t *testing.T) {
	base, shutdown := newPprofServer(t)
	t.Cleanup(shutdown)

	dir := t.TempDir()
	cfg := Config{TargetURL: base, OutDir: dir}
	path, err := CaptureGoroutine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CaptureGoroutine: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("goroutine profile is empty")
	}
}

func TestCaptureNoTarget(t *testing.T) {
	_, err := CaptureCPU(context.Background(), Config{})
	if err != ErrNoTarget {
		t.Errorf("CaptureCPU() err = %v, want ErrNoTarget", err)
	}
	_, err = CaptureHeap(context.Background(), Config{})
	if err != ErrNoTarget {
		t.Errorf("CaptureHeap() err = %v, want ErrNoTarget", err)
	}
	_, err = CaptureGoroutine(context.Background(), Config{})
	if err != ErrNoTarget {
		t.Errorf("CaptureGoroutine() err = %v, want ErrNoTarget", err)
	}
}

// TestProfileableInterface is a compile-time check that the Profileable
// interface is importable and has the expected shape. Servers satisfy
// it by implementing a ProfilePort() int method.
func TestProfileableInterface(t *testing.T) {
	var _ Profileable = profileableStub{}
}

type profileableStub struct{}

func (profileableStub) ProfilePort() int { return 0 }
