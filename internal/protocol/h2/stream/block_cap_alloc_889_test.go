package stream

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// celeris#889 item 37, the cost: a header block that is complete in one
// HEADERS frame and longer than the cap (headerBlockExpansion x the list bound)
// is a connection error (RFC 9113 section 10.5.1), and the processor does
// not decode a byte of it. Before, a 1 MiB frame of 4000-byte values was decoded in full
// (about its own size in allocations, up to 4.7x for short distinct literals,
// #979) and only then refused by the exact list bound.
func TestOneFrameBlockOverTheCapIsNotDecoded889(t *testing.T) {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range reqHeaders944() {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	for i := 0; hb.Len() < 1<<20-8000; i++ { // distinct incremental-indexing literals of 4000 bytes
		_ = enc.WriteField(hpack.HeaderField{Name: fmt.Sprintf("x-pad-%d", i), Value: fmt.Sprintf("%04d", i) + strings.Repeat("v", 3996)})
	}
	block := append([]byte(nil), hb.Bytes()...)
	if len(block) <= maxHeaderListSize*headerBlockExpansion || len(block) > 1<<20 {
		t.Fatalf("self-check: block of %d bytes", len(block))
	}
	for _, path := range []string{"raw", "frame"} {
		t.Run(path, func(t *testing.T) {
			p := newProc944()
			runtime.GC()
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			err := feed944(t, p, path, 1, true, block)
			runtime.ReadMemStats(&b)
			alloc := b.TotalAlloc - a.TotalAlloc
			// The "frame" path allocates the frame it reads in the test's own
			// framer (the engines' framer does the same); the processor's own
			// cost is read on the raw path, which reads nothing.
			if path == "raw" && alloc > 64<<10 {
				t.Errorf("a %d-byte one-frame header block over the cap allocated %d bytes (%.1fx): it was decoded", len(block), alloc, float64(alloc)/float64(len(block)))
			}
			if err == nil {
				t.Error("a one-frame header block over the cap was refused as a stream error, want the connection error")
			}
			goaways, rst := sinks889(p.Processor)
			if len(goaways) != 1 || goaways[0].code != http2.ErrCodeEnhanceYourCalm {
				t.Errorf("GOAWAYs %+v, want one ENHANCE_YOUR_CALM", goaways)
			}
			if len(rst) != 0 {
				t.Errorf("RST_STREAMs %+v, want none", rst)
			}
			if p.runs.Load() != 0 {
				t.Error("the handler ran")
			}
			t.Logf("block %d bytes -> allocated %d bytes", len(block), alloc)
		})
	}
}
