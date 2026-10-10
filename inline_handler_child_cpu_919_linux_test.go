//go:build linux

package celeris_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/platform/pintest"
)

// celeris#919: a handler that runs inline (AsyncHandlers false, the default) runs
// on an engine loop's thread, which the native engines pin to one CPU, and a
// process started from that thread inherits the one-CPU mask (os/exec clones the
// calling thread; SysProcAttr has no CPU field). The contract written on
// Config.AsyncHandlers has two ways to a child that has the process's mask:
// start the process from a goroutine of its own, or mark the route [Route.Async]
// (an async route's handler runs on a goroutine of its own). A route that only
// inherits the AsyncHandlers default is not one of them: it runs inline until a
// timed run of it blocks.
//
// The test starts the child both documented ways and checks that the child has
// the process's mask. The undocumented way (the child started on the handler's
// own thread, inline) must show a one-CPU child at least once: that is the
// premise, "the loops are pinned and a child inherits it", without which the two
// ways above would pass for any engine that pinned nothing.
func TestInlineHandlerChildKeepsTheProcessMask919(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 3*time.Minute)
		return
	}
	startMask, err := pintest.StartupMask()
	if err != nil {
		t.Fatalf("read the process's CPU mask: %v", err)
	}
	if n := pintest.CountCPUs(startMask); n < 2 {
		t.Skipf("the process may use %d CPU (%s): a one-CPU child cannot be told from one with the process's mask", n, startMask)
	}
	statusChild := func() (string, error) {
		out, err := exec.Command("cat", "/proc/self/status").Output()
		return cpusAllowedList919(string(out)), err
	}
	for _, arm := range keptArms(t) {
		if arm.async || (arm.engine != celeris.Epoll && arm.engine != celeris.IOUring) {
			continue
		}
		t.Run(arm.name, func(t *testing.T) {
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: arm.engine, Workers: 4})
				reply := func(c *celeris.Context, mask string, err error) error {
					if err != nil {
						return c.String(500, "%s", err.Error())
					}
					return c.String(200, "%s", mask)
				}
				// Documented way 1: the child is started from a goroutine of its own.
				srv.GET("/goroutine", func(c *celeris.Context) error {
					type res struct {
						mask string
						err  error
					}
					ch := make(chan res, 1)
					go func() {
						mask, err := statusChild()
						ch <- res{mask, err}
					}()
					r := <-ch
					return reply(c, r.mask, r.err)
				})
				// Documented way 2: a route marked .Async(), the child started
				// straight from the handler.
				srv.GET("/async", func(c *celeris.Context) error {
					mask, err := statusChild()
					return reply(c, mask, err)
				}).Async()
				// Not a way: an unmarked route on an inline server, the child
				// started straight from the handler.
				srv.GET("/inline", func(c *celeris.Context) error {
					mask, err := statusChild()
					return reply(c, mask, err)
				})
				return srv
			})
			defer stop()

			const rounds = 12
			for i := range rounds {
				for _, path := range []string{"/goroutine", "/async"} {
					if got := get919(t, addr, path); got != startMask {
						t.Errorf("request %d to %s: the child has CPU mask %q, want the process's %q", i, path, got, startMask)
					}
				}
			}
			// The premise. Each connection lands on a loop of SO_REUSEPORT's
			// choosing; with one loop pinned of four, a connection misses it
			// three times in four, so ask until a child shows the pin.
			const maxTries = 200
			single, tries := 0, 0
			for tries < maxTries && single == 0 {
				tries++
				if got := get919(t, addr, "/inline"); pintest.CountCPUs(got) == 1 {
					single++
				}
			}
			if single == 0 {
				t.Errorf("premise: none of %d children an inline handler started had a one-CPU mask, so no loop that served "+
					"them was pinned and the documented ways above show nothing", tries)
			}
			t.Logf("celeris919 RESULT engine=%s process_mask=%s documented_children=%d inline_child_on_one_cpu_after=%d_requests",
				arm.name, startMask, rounds*2, tries)
		})
	}
}

func cpusAllowedList919(status string) string {
	for line := range strings.SplitSeq(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == "Cpus_allowed_list" {
			return strings.TrimSpace(v)
		}
	}
	return "no Cpus_allowed_list"
}

// get919 fetches path on a new connection, so that every request lands on a
// loop of its own choosing (SO_REUSEPORT).
func get919(t *testing.T, addr, path string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
