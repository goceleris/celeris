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
// Config.AsyncHandlers is: start the process from a goroutine of its own, which
// never runs on the locked loop thread, and the child gets the process's mask.
//
// The test starts the child both ways and checks the way the documentation
// gives. How the other way (the child started on the loop's own thread) comes
// out is logged, not asserted: on a big.LITTLE host a loop the engine leaves
// unpinned (celeris#909) has no pin to hand on.
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
	for _, arm := range keptArms(t) {
		if arm.async || (arm.engine != celeris.Epoll && arm.engine != celeris.IOUring) {
			continue
		}
		t.Run(arm.name, func(t *testing.T) {
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: arm.engine, Workers: 4})
				// The documented way: the child is started from a goroutine of its own.
				srv.GET("/documented", func(c *celeris.Context) error {
					type res struct {
						out []byte
						err error
					}
					ch := make(chan res, 1)
					go func() {
						out, err := exec.Command("cat", "/proc/self/status").Output()
						ch <- res{out, err}
					}()
					r := <-ch
					if r.err != nil {
						return c.String(500, "%s", r.err.Error())
					}
					return c.String(200, "%s", cpusAllowedList919(string(r.out)))
				})
				// The other way: the child is started on the handler's own thread.
				srv.GET("/direct", func(c *celeris.Context) error {
					out, err := exec.Command("cat", "/proc/self/status").Output()
					if err != nil {
						return c.String(500, "%s", err.Error())
					}
					return c.String(200, "%s", cpusAllowedList919(string(out)))
				})
				return srv
			})
			defer stop()

			const rounds = 12
			single := 0
			for i := range rounds {
				if got := get919(t, addr, "/documented"); got != startMask {
					t.Errorf("request %d: a child started from a goroutine of its own has CPU mask %q, want the process's %q", i, got, startMask)
				}
				if got := get919(t, addr, "/direct"); pintest.CountCPUs(got) == 1 {
					single++
				}
			}
			t.Logf("celeris919 RESULT engine=%s process_mask=%s documented_way_children=%d direct_children_on_one_cpu=%d/%d",
				arm.name, startMask, rounds, single, rounds)
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
