//go:build linux

package celeris_test

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

	"github.com/goceleris/celeris"
)

// celeris#976 on every engine (the guard for the siblings, celeris RULE 61):
// the body of an Upgrade: h2c request is bounded by MaxRequestBodySize. The
// native engines refused such a request with 413 after about a MiB; the std
// engine read all of it (32 MiB, +74 MiB allocated) and answered 101.
func TestUpgradeBodyBoundedByMaxRequestBodySize976(t *testing.T) {
	const n = 32 << 20
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer964(t, celeris.Config{Engine: e.eng, Protocol: celeris.Auto, MaxRequestBodySize: 4096})
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			chunk := make([]byte, 1<<20) // allocated before the baseline
			runtime.GC()
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			head := fmt.Sprintf("POST /h HTTP/1.1\r\nHost: a\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
				"HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\nContent-Length: %d\r\n\r\n", n)
			if _, err := io.WriteString(c, head); err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int64
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				for sent.Load() < n {
					w, err := c.Write(chunk)
					sent.Add(int64(w))
					if err != nil {
						return
					}
				}
			}()
			line, _ := bufio.NewReader(c).ReadString('\n')
			_ = c.Close()
			<-writerDone
			runtime.ReadMemStats(&m1)
			first := strings.TrimSpace(line)
			alloc := m1.TotalAlloc - m0.TotalAlloc
			t.Logf("MEASURE engine=%s sent=%d first=%q TotalAllocDeltaMiB=%d (the test process: server and client)", e.name, sent.Load(), first, alloc>>20)
			if !strings.HasPrefix(first, "HTTP/1.1 413") {
				t.Fatalf("%s: a %d byte Upgrade: h2c body with MaxRequestBodySize 4096 was answered %q, want 413", e.name, n, first)
			}
			if s := sent.Load(); s > n/4 {
				t.Fatalf("%s: the server took %d of %d body bytes before refusing, want at most %d", e.name, s, n, n/4)
			}
			if alloc > n/2 {
				t.Fatalf("%s: refusing the request allocated %d MiB, want at most %d MiB", e.name, alloc>>20, (n/2)>>20)
			}
		})
	}
}
