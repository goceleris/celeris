package celeris

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// startFailures are the ways a Start can fail before any engine runs: in
// doPrepare, which returns before Listen is ever called.
func startFailures() []struct {
	name string
	cfg  Config
} {
	return []struct {
		name string
		cfg  Config
	}{
		{"config validation", Config{Engine: Std, MaxHeaderBytes: 100}},
		{"TrustedProxies", Config{Engine: Std, TrustedProxies: []string{"not-an-ip"}}},
		// No factory knows this engine type, so createEngine fails, after
		// doPrepare has opened the CPU monitor.
		{"createEngine", Config{Engine: EngineType(99)}},
	}
}

// listenerStarts are the entry points that take a caller's listener.
func listenerStarts() []struct {
	name  string
	start func(s *Server, ln net.Listener) error
} {
	return []struct {
		name  string
		start func(s *Server, ln net.Listener) error
	}{
		{"StartWithListener", func(s *Server, ln net.Listener) error { return s.StartWithListener(ln) }},
		{"StartWithListenerAndContext", func(s *Server, ln net.Listener) error {
			return s.StartWithListenerAndContext(context.Background(), ln)
		}},
	}
}

// TestFailedStartClosesTheSuppliedListener pins celeris#737. StartWithListener
// and StartWithListenerAndContext tell the caller not to close the listener
// it hands over, and a start that failed before its engine ran returned the
// error with that listener still open: bound and listening, so the kernel kept
// completing handshakes into a backlog nothing would ever accept. It must be
// closed, and a client dialling its address refused.
//
// A second call on the same server returns the first call's error without
// running doPrepare again, and must close the listener it is handed as well.
func TestFailedStartClosesTheSuppliedListener(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, f := range startFailures() {
		for _, entry := range listenerStarts() {
			t.Run(f.name+"/"+entry.name, func(t *testing.T) {
				cfg := f.cfg
				cfg.Logger = quiet
				s := New(cfg)
				for call := 1; call <= 2; call++ {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatalf("listen: %v", err)
					}
					addr := ln.Addr().String()
					startErr := entry.start(s, ln)
					if startErr == nil || errors.Is(startErr, ErrAlreadyStarted) {
						_ = ln.Close()
						t.Fatalf("call %d: %s returned %v, want the start failure", call, entry.name, startErr)
					}
					// Closing a listener that is already closed reports
					// net.ErrClosed; closing an open one succeeds (and tidies up).
					if cerr := ln.Close(); !errors.Is(cerr, net.ErrClosed) {
						t.Errorf("call %d: %s failed (%v) and left the supplied listener open (celeris#737)", call, entry.name, startErr)
					}
					if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
						_ = c.Close()
						t.Errorf("call %d: after the failed start a dial to the listener's address %s still connected", call, addr)
					}
				}
			})
		}
	}
}

// TestErrAlreadyStartedLeavesTheListenerToTheCaller is the boundary of the
// celeris#737 fix: a call that finds the server already started does not own
// the listener it was handed. It may be the listener the running server
// serves on (std uses it directly), so closing it would stop that server.
func TestErrAlreadyStartedLeavesTheListenerToTheCaller(t *testing.T) {
	for _, entry := range listenerStarts() {
		t.Run(entry.name, func(t *testing.T) {
			s := New(Config{})
			s.startOnce.Do(func() {
				var fe engine.Engine = &fakeEngine{}
				s.engineRef.Store(&fe)
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			if err := entry.start(s, ln); !errors.Is(err, ErrAlreadyStarted) {
				_ = ln.Close()
				t.Fatalf("%s on a started server returned %v, want ErrAlreadyStarted", entry.name, err)
			}
			if cerr := ln.Close(); cerr != nil {
				t.Errorf("%s returned ErrAlreadyStarted and closed the caller's listener (Close: %v)", entry.name, cerr)
			}
		})
	}
}

// TestFailedStartClosesTheCPUMonitor pins the other half of celeris#737:
// doPrepare opens the CPU monitor (a /proc/stat descriptor on Linux) before it
// creates the engine, and only Shutdown closed it, which a caller whose Start
// failed has no reason to call. Every entry point shares doPrepare.
func TestFailedStartClosesTheCPUMonitor(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	entries := []struct {
		name  string
		start func(s *Server) error
	}{
		{"Start", func(s *Server) error { return s.Start() }},
		{"StartWithContext", func(s *Server) error { return s.StartWithContext(context.Background()) }},
		{"StartWithListener", func(s *Server) error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			return s.StartWithListener(ln)
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			s := New(Config{Engine: EngineType(99), Addr: "127.0.0.1:0", Logger: quiet})
			err := entry.start(s)
			if err == nil {
				t.Fatalf("%s with an unknown engine type returned nil", entry.name)
			}
			s.cpuMonMu.Lock()
			mon := s.cpuMon
			s.cpuMonMu.Unlock()
			if mon != nil {
				t.Errorf("%s failed to create its engine (%v) and left the CPU monitor open (celeris#737)", entry.name, err)
				s.closeCPUMonitor()
			}
		})
	}
}

// TestStartWithoutShutdownReleasesWhatItOpened covers the rest of the
// celeris#737 class: a Start that published its engine and ran Listen, but
// ends with no Shutdown to come. Listen failed (the address is taken), or
// Shutdown was called before Start, so Listen ran on a context that was
// already cancelled and returned at once. doPrepare had opened the CPU monitor
// and started the settle re-opener (celeris#592), and only Shutdown released
// them: a descriptor and a goroutine per such Start, for the life of the
// process. Every Start* entry point now releases both once Listen has
// returned.
func TestStartWithoutShutdownReleasesWhatItOpened(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()
	busyAddr := busy.Addr().String()

	closedListener := func(t *testing.T) net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		_ = ln.Close()
		return ln
	}
	openListener := func(t *testing.T) net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		return ln
	}
	cases := []struct {
		name string
		addr string
		// listener, when set, makes the listener handed to start.
		listener func(t *testing.T) net.Listener
		// shutdownFirst: Shutdown is called before Start.
		shutdownFirst bool
		start         func(s *Server, ln net.Listener) error
		wantErr       bool
	}{
		{"Listen fails/Start", busyAddr, nil, false, func(s *Server, _ net.Listener) error { return s.Start() }, true},
		{"Listen fails/StartWithContext", busyAddr, nil, false, func(s *Server, _ net.Listener) error {
			return s.StartWithContext(context.Background())
		}, true},
		{"Listen fails/StartWithListener", "", closedListener, false, func(s *Server, ln net.Listener) error {
			return s.StartWithListener(ln)
		}, true},
		{"Shutdown first/Start", "127.0.0.1:0", nil, true, func(s *Server, _ net.Listener) error { return s.Start() }, false},
		{"Shutdown first/StartWithContext", "127.0.0.1:0", nil, true, func(s *Server, _ net.Listener) error {
			return s.StartWithContext(context.Background())
		}, false},
		{"Shutdown first/StartWithListenerAndContext", "", openListener, true, func(s *Server, ln net.Listener) error {
			return s.StartWithListenerAndContext(context.Background(), ln)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// AsyncHandlers makes /s an adaptive route, and only adaptive
			// routes start the settle re-opener.
			s := New(Config{Engine: Std, Addr: tc.addr, AsyncHandlers: true, Logger: quiet})
			s.GET("/s", noopHandler)
			if !s.router.adaptiveRoutes["/s"] {
				t.Fatal("precondition: /s must be adaptive, or the re-opener never starts")
			}
			if tc.shutdownFirst {
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatalf("Shutdown before Start: %v", err)
				}
			}
			var ln net.Listener
			if tc.listener != nil {
				ln = tc.listener(t)
			}
			done := make(chan error, 1)
			go func() { done <- tc.start(s, ln) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Start did not return")
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("Start returned %v, want an error: %v", err, tc.wantErr)
			}
			if s.loadEngine() == nil {
				t.Fatal("precondition: the engine was never published, so this is not the case under test")
			}
			if reopenerRunning(s.router) {
				t.Errorf("Start returned (%v) with the settle re-opener still running and no Shutdown to come (celeris#737)", err)
				s.router.stopSettleReopener()
			}
			s.cpuMonMu.Lock()
			mon := s.cpuMon
			s.cpuMonMu.Unlock()
			if mon != nil {
				t.Errorf("Start returned (%v) with the CPU monitor still open and no Shutdown to come (celeris#737)", err)
				s.closeCPUMonitor()
			}
		})
	}
}
