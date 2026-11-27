//go:build linux

package celeris_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

// oneCPUChildEnv names the child run of TestOneCPUEnginesServeGetAndWebSocket:
// the test binary started again under taskset, with one allowed CPU.
const oneCPUChildEnv = "CELERIS_TEST_ONE_CPU_CHILD"

// TestOneCPUEnginesServeGetAndWebSocket is the one-CPU check: a process whose
// allowed CPUs are one (a container with a one-CPU cpuset, taskset -c N), with
// Config.Workers UNSET, starts on each engine (std, epoll, io_uring, adaptive)
// and serves a plain GET and a WebSocket echo, with no error. The loops the
// native engines start there are the minimum of 2 (Config.Workers: a set value
// below 2 is refused), both pinned to the one CPU.
//
// The test starts its own binary again under taskset, so the child has one
// allowed CPU and GOMAXPROCS 1, as such a container has; it is skipped where
// taskset is not installed. The child asserts that premise before it runs the
// engines, so a taskset that did not bind fails the test instead of passing it
// on a many-CPU process.
func TestOneCPUEnginesServeGetAndWebSocket(t *testing.T) {
	if os.Getenv(oneCPUChildEnv) == "" {
		taskset, err := exec.LookPath("taskset")
		if err != nil {
			t.Skip("taskset is not installed: the one-CPU process cannot be made here")
		}
		var set unix.CPUSet
		if err := unix.SchedGetaffinity(0, &set); err != nil {
			t.Fatalf("sched_getaffinity: %v", err)
		}
		cpu := -1
		for i := 0; i < 1024; i++ {
			if set.IsSet(i) {
				cpu = i
				break
			}
		}
		if cpu < 0 {
			t.Fatal("no allowed CPU")
		}
		cmd := exec.Command(taskset, "-c", strconv.Itoa(cpu), os.Args[0],
			"-test.run=^TestOneCPUEnginesServeGetAndWebSocket$", "-test.v", "-test.timeout=150s")
		cmd.Env = append(os.Environ(), oneCPUChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("child output:\n%s", out)
		if err != nil {
			t.Fatalf("the one-CPU child failed: %v", err)
		}
		return
	}

	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	if n := set.Count(); n != 1 || runtime.GOMAXPROCS(0) != 1 {
		t.Fatalf("premise: the child has %d allowed CPUs and GOMAXPROCS %d, want 1 and 1", n, runtime.GOMAXPROCS(0))
	}
	for _, e := range drainEngines806 {
		t.Run(e.name, func(t *testing.T) {
			// Workers is not set: startServer760 builds the Config from the engine alone.
			srv := startDrainServer806(t, e, 5*time.Second, 0, nil, func(s *celeris.Server) {
				s.GET("/ws", websocket.New(websocket.Config{Handler: func(c *websocket.Conn) {
					for {
						mt, msg, err := c.ReadMessage()
						if err != nil {
							return
						}
						if err := c.WriteMessage(mt, msg); err != nil {
							return
						}
					}
				}}))
			})
			// The plain GET is the /ping startServer760 waited for. Echo three frames over a WebSocket.
			c, err := net.DialTimeout("tcp", srv.addr, 3*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			if err := echoWebSocketOneCPU(c); err != nil {
				t.Fatalf("%s on one CPU: websocket echo: %v", e.name, err)
			}
			if w := srv.s.EngineInfo().Metrics.Workers; e.eng != celeris.Std && w < 1 {
				t.Errorf("%s: no loop started (Metrics.Workers = %d)", e.name, w)
			}
		})
	}
}

// echoWebSocketOneCPU upgrades c to a WebSocket and echoes three masked text
// frames through it.
func echoWebSocketOneCPU(c net.Conn) error {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", base64.StdEncoding.EncodeToString(key)); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		return fmt.Errorf("handshake answered %q (%v)", status, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if l == "\r\n" {
			break
		}
	}
	for i := range 3 {
		msg := "hello-" + strconv.Itoa(i)
		mask := [4]byte{1, 2, 3, 4}
		frame := []byte{0x81, byte(0x80 | len(msg))}
		frame = append(frame, mask[:]...)
		for j := 0; j < len(msg); j++ {
			frame = append(frame, msg[j]^mask[j%4])
		}
		if _, err := c.Write(frame); err != nil {
			return err
		}
		h := make([]byte, 2)
		if _, err := io.ReadFull(br, h); err != nil {
			return err
		}
		body := make([]byte, h[1]&0x7f)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		if string(body) != msg {
			return fmt.Errorf("echoed %q, want %q", body, msg)
		}
	}
	return nil
}
