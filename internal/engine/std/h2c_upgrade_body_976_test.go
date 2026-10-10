package std

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#976: the h2c front end read the body of an RFC 7540 3.2 upgrade
// request whole (io.ReadAll) before it hijacked the connection, so Config.
// MaxRequestBodySize, which Bridge applies afterwards, never bounded it: a
// client that asked for the upgrade with a 32 MiB body made the server
// buffer all of it and answer 101. The native engines refuse such a request
// with 413 after about a MiB. Like the #964 tests, these run on every
// platform: std is the only engine on macOS and Windows.

// upgradeBodyOutcome976 is what a client saw when it sent an Upgrade: h2c
// POST with a body of n bytes.
type upgradeBodyOutcome976 struct {
	first      string // the status line of the first reply ("" if none)
	sent       int64  // body bytes the client got onto the connection
	allocBytes uint64 // server+client TotalAlloc growth in this process
}

func sendUpgradeBody976(t *testing.T, addr string, n int64) upgradeBodyOutcome976 {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	chunk := make([]byte, 1<<20) // allocated before the baseline below
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	head := fmt.Sprintf("POST /h HTTP/1.1\r\nHost: std.test\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\nContent-Length: %d\r\n\r\n", n)
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatalf("write head: %v", err)
	}
	var sent atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for sent.Load() < n {
			k := int64(len(chunk))
			if rest := n - sent.Load(); rest < k {
				k = rest
			}
			w, err := c.Write(chunk[:k])
			sent.Add(int64(w))
			if err != nil {
				return
			}
		}
	}()
	line, _ := bufio.NewReader(c).ReadString('\n')
	_ = c.Close() // ends the writer if the server is still reading
	<-writerDone
	runtime.ReadMemStats(&m1)
	return upgradeBodyOutcome976{first: strings.TrimSpace(line), sent: sent.Load(), allocBytes: m1.TotalAlloc - m0.TotalAlloc}
}

// TestAutoUpgradeBodyBoundedByMaxRequestBodySize976: a 32 MiB body on an
// Upgrade: h2c request, MaxRequestBodySize 4096, Protocol Auto and the
// default (upgrade on): refused with 413 after a bounded read and a bounded
// allocation. Before the fix: 101, all 33554432 bytes read, +74 MiB.
func TestAutoUpgradeBodyBoundedByMaxRequestBodySize976(t *testing.T) {
	const n = 32 << 20
	_, addr := startAutoEngine(t, func(c *resource.Config) { c.MaxRequestBodySize = 4096 })
	o := sendUpgradeBody976(t, addr, n)
	t.Logf("MEASURE std first=%q sent=%d TotalAllocDeltaMiB=%d", o.first, o.sent, o.allocBytes>>20)
	if !strings.HasPrefix(o.first, "HTTP/1.1 413") {
		t.Fatalf("a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", n, o.first)
	}
	if o.sent > n/2 {
		t.Fatalf("the server took %d of %d body bytes before refusing, want at most %d (a bounded read)", o.sent, n, n/2)
	}
	if o.allocBytes > n/2 {
		t.Fatalf("serving the refused request allocated %d MiB, want at most %d MiB", o.allocBytes>>20, (n/2)>>20)
	}
}

// TestH2CUpgradeBodyBoundedByMaxRequestBodySize976 is the same on Protocol
// H2C, where the upgrade is the only way in from HTTP/1.1.
func TestH2CUpgradeBodyBoundedByMaxRequestBodySize976(t *testing.T) {
	const n = 32 << 20
	e, addr := startH2CEngineWith(t, newPathHandler(), nil, func(c *resource.Config) {
		c.Protocol = engine.H2C
		c.MaxRequestBodySize = 4096
	})
	o := sendUpgradeBody976(t, addr, n)
	t.Logf("MEASURE std(h2c) first=%q sent=%d TotalAllocDeltaMiB=%d", o.first, o.sent, o.allocBytes>>20)
	if !strings.HasPrefix(o.first, "HTTP/1.1 413") {
		t.Fatalf("a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", n, o.first)
	}
	if o.sent > n/2 {
		t.Fatalf("the server took %d of %d body bytes before refusing, want at most %d", o.sent, n, n/2)
	}
	// Bridge counts a refused body as a request-body error; so does this.
	deadline := time.Now().Add(5 * time.Second)
	for e.Metrics().ErrorRequestBody == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.Metrics().ErrorRequestBody; got != 1 {
		t.Fatalf("ErrorRequestBody = %d after one refused upgrade body, want 1", got)
	}
}

// TestUpgradeBodyWithinLimitStillUpgrades976 is the control for the bound: a
// body within MaxRequestBodySize is served as stream 1 after the 101, byte
// for byte, and an unlimited config (-1) takes any size.
func TestUpgradeBodyWithinLimitStillUpgrades976(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		body  int64
	}{
		{"at-the-limit", 4096, 4096},
		{"under-the-limit", 4096, 100},
		{"unlimited", -1, 1 << 20},
		{"no-body", 4096, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := startAutoEngine(t, func(c *resource.Config) { c.MaxRequestBodySize = tc.limit })
			o := sendUpgradeBody976(t, addr, tc.body)
			if !strings.HasPrefix(o.first, "HTTP/1.1 101") {
				t.Fatalf("a %d byte Upgrade: h2c body under limit %d was answered %q, want 101", tc.body, tc.limit, o.first)
			}
		})
	}
}
