package std

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"

	"golang.org/x/net/http2"
)

// The tests below pin celeris#964 for the std engine on every platform (the
// root-package test of the same name is linux-only and std is the only
// engine on macOS and Windows): Protocol Auto with the h2c upgrade resolved
// to false serves a request that asks for the RFC 7540 3.2 upgrade as plain
// HTTP/1.1, keeps prior-knowledge h2c, and leaves the default (and an
// explicit true) upgrading.

// upgradeFirstLine sends an Upgrade: h2c request to addr and returns the
// status line of the first reply.
func upgradeFirstLine(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	req := "GET /upgrade HTTP/1.1\r\nHost: std.test\r\n" +
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: \r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	return strings.TrimSpace(line)
}

func startAutoEngine(t *testing.T, resolve func(*resource.Config)) (*pathHandler, string) {
	t.Helper()
	h := newPathHandler()
	_, addr := startH2CEngineWith(t, h, nil, func(c *resource.Config) {
		c.Protocol = engine.Auto
		if resolve != nil {
			resolve(c)
		}
	})
	return h, addr
}

func TestAutoWithUpgradeDisabledServesUpgradeRequestAsHTTP1(t *testing.T) {
	h, addr := startAutoEngine(t, func(c *resource.Config) { c.SetH2Upgrade(false) })

	if got := upgradeFirstLine(t, addr); got != "HTTP/1.1 200 OK" {
		t.Fatalf("Auto + upgrade disabled answered an Upgrade: h2c request with %q, want %q", got, "HTTP/1.1 200 OK")
	}
	if got := h.snapshot(); len(got) != 1 || got[0] != "/upgrade" {
		t.Fatalf("handler saw %v, want exactly [/upgrade]: the request must be served once, as HTTP/1.1", got)
	}
}

func TestAutoWithUpgradeDisabledKeepsPriorKnowledgeH2C(t *testing.T) {
	_, addr := startAutoEngine(t, func(c *resource.Config) { c.SetH2Upgrade(false) })

	r := dialRawH2(t, addr, false)
	r.get(1, "/pk")
	ev, _, ok := r.waitFor(5*time.Second, func(ev h2Event) bool { return ev.typ == http2.FrameHeaders })
	if !ok || ev.status != "200" {
		t.Fatalf("prior-knowledge h2c on Auto + upgrade disabled: ok=%v event=%+v, want a 200 HEADERS frame", ok, ev)
	}
}

func TestAutoWithUpgradeDisabledServesPlainHTTP1(t *testing.T) {
	_, addr := startAutoEngine(t, func(c *resource.Config) { c.SetH2Upgrade(false) })

	resp, err := http.Get("http://" + addr + "/plain")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || resp.ProtoMajor != 1 {
		t.Fatalf("plain HTTP/1.1 got %d on HTTP/%d, want 200 on HTTP/1", resp.StatusCode, resp.ProtoMajor)
	}
}

func TestAutoUpgradeStillAnswers101WhenNotDisabled(t *testing.T) {
	cases := map[string]func(*resource.Config){
		"default":       nil,
		"explicit-true": func(c *resource.Config) { c.SetH2Upgrade(true) },
	}
	for name, resolve := range cases {
		t.Run(name, func(t *testing.T) {
			_, addr := startAutoEngine(t, resolve)
			if got := upgradeFirstLine(t, addr); !strings.HasPrefix(got, "HTTP/1.1 101") {
				t.Fatalf("Auto (%s) answered an Upgrade: h2c request with %q, want 101", name, got)
			}
		})
	}
}
