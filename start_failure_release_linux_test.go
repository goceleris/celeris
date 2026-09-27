//go:build linux

package celeris_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/goceleris/celeris"
)

// TestFailedStartsLeaveNoProcStatDescriptor is celeris#737 as it was
// measured: twenty starts that fail to create their engine, with GC off so no
// finalizer can close a leaked descriptor, must leave no /proc/stat
// descriptor open. doPrepare opens one for the CPU monitor before it creates
// the engine, and only Shutdown closed it. The count is of descriptors whose
// link is /proc/stat, so descriptors other tests open or close meanwhile
// cannot move it.
func TestFailedStartsLeaveNoProcStatDescriptor(t *testing.T) {
	const starts = 20
	prev := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(prev)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	entries := []struct {
		name  string
		start func(s *celeris.Server) error
	}{
		{"Start", func(s *celeris.Server) error { return s.Start() }},
		{"StartWithContext", func(s *celeris.Server) error { return s.StartWithContext(context.Background()) }},
		{"StartWithListener", func(s *celeris.Server) error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			return s.StartWithListener(ln)
		}},
		{"StartWithListenerAndContext", func(s *celeris.Server) error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			return s.StartWithListenerAndContext(context.Background(), ln)
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			before := procStatDescriptors(t)
			for range starts {
				// No factory knows engine type 99, so createEngine fails.
				s := celeris.New(celeris.Config{Engine: celeris.EngineType(99), Addr: "127.0.0.1:0", Logger: quiet})
				if err := entry.start(s); err == nil {
					t.Fatalf("%s with an unknown engine type returned nil", entry.name)
				}
			}
			after := procStatDescriptors(t)
			t.Logf("%s: /proc/stat descriptors %d before %d failed starts, %d after", entry.name, before, starts, after)
			if after != before {
				t.Errorf("%s: %d failed starts left %d /proc/stat descriptors open (celeris#737)", entry.name, starts, after-before)
			}
		})
	}
}

// TestStartsWithoutShutdownLeaveNoProcStatDescriptor is the rest of the
// celeris#737 class, measured the same way: Starts that publish their engine
// and run Listen but end with no Shutdown to come, because Listen fails (the
// address is taken by a socket without SO_REUSEPORT) or because Shutdown was
// called before Start. On std and on epoll, which binds per worker.
func TestStartsWithoutShutdownLeaveNoProcStatDescriptor(t *testing.T) {
	const starts = 20
	prev := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(prev)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()
	busyAddr := busy.Addr().String()

	cases := []struct {
		name          string
		cfg           celeris.Config
		shutdownFirst bool
		wantErr       bool
		start         func(s *celeris.Server) error
	}{
		{"Listen fails/std/Start", celeris.Config{Engine: celeris.Std, Addr: busyAddr}, false, true,
			func(s *celeris.Server) error { return s.Start() }},
		{"Listen fails/epoll/StartWithContext", celeris.Config{Engine: celeris.Epoll, Addr: busyAddr, Workers: 2}, false, true,
			func(s *celeris.Server) error { return s.StartWithContext(context.Background()) }},
		{"Shutdown first/std/StartWithContext", celeris.Config{Engine: celeris.Std, Addr: "127.0.0.1:0"}, true, false,
			func(s *celeris.Server) error { return s.StartWithContext(context.Background()) }},
		{"Shutdown first/epoll/StartWithListener", celeris.Config{Engine: celeris.Epoll, Workers: 2}, true, false,
			func(s *celeris.Server) error {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					return err
				}
				return s.StartWithListener(ln)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := procStatDescriptors(t)
			for i := range starts {
				cfg := tc.cfg
				cfg.Logger = quiet
				s := celeris.New(cfg)
				if tc.shutdownFirst {
					if err := s.Shutdown(context.Background()); err != nil {
						t.Fatalf("Shutdown before Start: %v", err)
					}
				}
				if err := tc.start(s); (err != nil) != tc.wantErr {
					t.Fatalf("start %d: Start returned %v, want an error: %v", i, err, tc.wantErr)
				}
			}
			after := procStatDescriptors(t)
			t.Logf("%s: /proc/stat descriptors %d before %d Starts, %d after", tc.name, before, starts, after)
			if after != before {
				t.Errorf("%s: %d Starts with no Shutdown to come left %d /proc/stat descriptors open (celeris#737)", tc.name, starts, after-before)
			}
		})
	}
}

// procStatDescriptors counts this process's descriptors open on /proc/stat.
func procStatDescriptors(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range ents {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == "/proc/stat" {
			n++
		}
	}
	return n
}
