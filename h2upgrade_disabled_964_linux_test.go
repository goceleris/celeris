//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// The request that asks for the RFC 7540 section 3.2 upgrade. The
// HTTP2-Settings value is a valid SETTINGS payload (MAX_CONCURRENT_STREAMS,
// INITIAL_WINDOW_SIZE and one more), so a server that honours the upgrade has
// nothing to reject.
const upgradeRequest964 = "GET /h HTTP/1.1\r\n" +
	"Host: example.com\r\n" +
	"Connection: Upgrade, HTTP2-Settings\r\n" +
	"Upgrade: h2c\r\n" +
	"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n" +
	"\r\n"

const plainRequest964 = "GET /h HTTP/1.1\r\nHost: example.com\r\n\r\n"

func bptr964(v bool) *bool { return &v }

// startServer964 starts a server with cfg on a free port and returns its
// address. Engine and EnableH2Upgrade are the point of the test; Protocol is
// whatever cfg says (the zero value is Auto).
func startServer964(t *testing.T, cfg celeris.Config) string {
	t.Helper()
	retryUntil := time.Now().Add(30 * time.Second)
	for tries := 1; ; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s := celeris.New(cfg)
		s.GET("/h", func(c *celeris.Context) error { return c.String(http.StatusOK, "h1-answer") })
		s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
		startDone := make(chan error, 1)
		go func() { startDone <- s.Start() }()
		err = waitReady761(addr, startDone)
		if err == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(ctx)
				select {
				case <-startDone:
				case <-time.After(15 * time.Second):
					t.Errorf("Start did not return within 15s of Shutdown")
				}
			})
			return addr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(retryUntil) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start (try %d): %v", tries, err)
	}
}

// TestEnableH2UpgradeFalseDisablesUpgradeOnAuto964 drives celeris#964 on every
// engine over a real connection: Config.EnableH2Upgrade = &false on
// Protocol Auto must serve a request that asks for the h2c upgrade as plain
// HTTP/1.1, as the field's documentation promises. On main,
// resource.Config.WithDefaults turned the false back into true on Auto, so
// epoll and io_uring answered 101 Switching Protocols (and the std engine,
// which does not read the flag at all, still does).
//
// The controls pin what must not change: nil (the default) and &true on Auto
// still upgrade, and &false must not take prior-knowledge HTTP/2 away from
// Auto, which is not the upgrade.
func TestEnableH2UpgradeFalseDisablesUpgradeOnAuto964(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			cases := []struct {
				name    string
				enable  *bool
				upgrade bool
			}{
				{"nil-default-upgrades", nil, true},
				{"true-upgrades", bptr964(true), true},
				{"false-serves-http1", bptr964(false), false},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					addr := startServer964(t, celeris.Config{
						Engine:          e.eng,
						Protocol:        celeris.Auto,
						EnableH2Upgrade: tc.enable,
					})
					checkUpgrade964(t, addr, tc.upgrade)
				})
			}

			t.Run("false-keeps-prior-knowledge-h2c", func(t *testing.T) {
				addr := startServer964(t, celeris.Config{
					Engine:          e.eng,
					Protocol:        celeris.Auto,
					EnableH2Upgrade: bptr964(false),
				})
				p := new(http.Protocols)
				p.SetUnencryptedHTTP2(true)
				tr := &http.Transport{Protocols: p, DisableCompression: true}
				cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
				defer cl.CloseIdleConnections()
				resp, err := cl.Get("http://" + addr + "/h")
				if err != nil {
					t.Fatalf("prior-knowledge h2c GET: %v", err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.ProtoMajor != 2 || resp.StatusCode != 200 || string(body) != "h1-answer" {
					t.Fatalf("prior-knowledge h2c: %s %d %q, want HTTP/2.0 200 \"h1-answer\"",
						resp.Proto, resp.StatusCode, body)
				}
			})
		})
	}
}

// checkUpgrade964 sends the upgrade request and requires either a 101 (wantUpgrade)
// or an ordinary HTTP/1.1 200 with the handler's body, followed by a second plain
// request on the same connection, which only works while the connection is
// still HTTP/1.1 (an upgraded one answers with HTTP/2 frames).
func checkUpgrade964(t *testing.T, addr string, wantUpgrade bool) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)

	if _, err := io.WriteString(c, upgradeRequest964); err != nil {
		t.Fatalf("write: %v", err)
	}
	if wantUpgrade {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read status line: %v", err)
		}
		if !strings.HasPrefix(line, "HTTP/1.1 101 ") {
			t.Fatalf("upgrade request answered %q, want 101 Switching Protocols", strings.TrimSpace(line))
		}
		return
	}
	for i, req := range []string{"", plainRequest964} {
		if req != "" {
			if _, err := io.WriteString(c, req); err != nil {
				t.Fatalf("write plain request: %v", err)
			}
		}
		what := fmt.Sprintf("response %d", i+1)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			peek, _ := br.Peek(br.Buffered())
			t.Fatalf("%s: not an HTTP/1.1 response: %v (next bytes %q)", what, err, peek)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.ProtoMajor != 1 || string(body) != "h1-answer" {
			t.Fatalf("%s: %s %d %q, want HTTP/1.1 200 \"h1-answer\" (no 101, no HTTP/2)",
				what, resp.Proto, resp.StatusCode, body)
		}
	}
}
