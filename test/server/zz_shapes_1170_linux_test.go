//go:build linux

package celeris_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ZZ: the shapes of review r10 F1. SHAPES="n,sizeMiB,bps,wt,idleMs;..." (temporary).
func TestZZShapes1170(t *testing.T) {
	spec := os.Getenv("SHAPES")
	if spec == "" {
		t.Skip("SHAPES unset")
	}
	for _, sh := range strings.Split(spec, ";") {
		f := strings.Split(sh, ",")
		n, _ := strconv.Atoi(f[0])
		mib, _ := strconv.Atoi(f[1])
		bps, _ := strconv.ParseFloat(f[2], 64)
		wt, _ := time.ParseDuration(f[3])
		idle, _ := strconv.Atoi(f[4])
		for _, e := range nativeEngines889() {
			if e.name == "io_uring" && wt < 5*time.Second {
				continue
			}
			t.Run(fmt.Sprintf("%s/%dx%dMiB@%.0f/WT%v/idle%d", e.name, n, mib, bps, wt, idle), func(t *testing.T) {
				var p pool1170
				addr := p.server(t, e.eng, -1, -1, wt)
				cl := goTransport1170(bps)
				warm, _ := cl.Get("http://" + addr + "/pool?n=1")
				if warm != nil {
					_ = warm.Body.Close()
				}
				time.Sleep(time.Duration(idle) * time.Millisecond)
				ends, at := downloadsOn1170(t, cl, addr, "/pool", n, mib<<20, 300*time.Second)
				cut := 0
				for _, r := range ends {
					if r != "complete" {
						cut++
					}
				}
				t.Logf("ZZ %s %dx%dMiB@%.0f WT%v idle%d: cut %d of %d; %s; last %v", e.name, n, mib, bps, wt, idle, cut, n, p.logs.summary(), at)
			})
		}
	}
}
