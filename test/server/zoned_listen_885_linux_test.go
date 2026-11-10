//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestZonedListenAddressServesOnEveryEngine885 (checklist #885 item 54): a
// Config.Addr with an IPv6 zone ("[fe80::1234%dz0]:port"), which net.Listen and
// so the std engine bind, starts the native engines too, through the exported
// API (celeris.New, Start), not only through the engine packages: parseAddr of
// epoll and io_uring used to refuse the zone ("invalid addr"). Adaptive starts
// both of them on the address, with its port hold beside them. The server must
// answer a request sent to the zoned address and report that address, zone
// included, as std does.
//
// It skips without an up, non-loopback interface that has an fe80:: address,
// and fails under CELERIS_REQUIRE_ZONED_PEER=1 (see zonedTarget890).
func TestZonedListenAddressServesOnEveryEngine885(t *testing.T) {
	host := zonedTarget890(t)
	arms := append(keptArms(t), keptArm{"adaptive", celeris.Adaptive, false})
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			// A free port on the zoned address: std binds it, then lets it go.
			probe, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Fatalf("std cannot listen on %s: %v", host, err)
			}
			port := probe.Addr().(*net.TCPAddr).Port
			_ = probe.Close()
			addr := net.JoinHostPort(host, strconv.Itoa(port))

			s := celeris.New(celeris.Config{Engine: arm.engine, AsyncHandlers: arm.async, Addr: addr})
			s.GET("/", func(c *celeris.Context) error { return c.String(http.StatusOK, "zoned") })
			done := make(chan error, 1)
			go func() { done <- s.Start() }()
			returned := false // Start has returned and its result was read below
			t.Cleanup(func() {
				if returned {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			deadline := time.Now().Add(15 * time.Second)
			for s.Addr() == nil {
				select {
				case err := <-done:
					returned = true
					t.Fatalf("Start on %s returned before the server was ready: %v", addr, err)
				case <-time.After(5 * time.Millisecond):
				}
				if time.Now().After(deadline) {
					t.Fatalf("no address published for %s within 15s", addr)
				}
			}
			// The native engines name the zone of the address they bound
			// (bindiag.BoundAddr). std's own Addr() is not asserted: it is the
			// oracle here, and on a hosted runner it prints the address without
			// the zone (CI run 40425220210), which is not this change's to fix.
			if got := s.Addr().String(); got != addr && arm.engine != celeris.Std {
				t.Errorf("Addr() = %q, want %q (std's form, the zone included)", got, addr)
			} else if got != addr {
				t.Logf("std reports %q for %q", got, addr)
			}
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatalf("dial %s: %v", addr, err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "zoned") {
				t.Errorf("answered %d %q, want 200 zoned", resp.StatusCode, body)
			}
		})
	}
}
