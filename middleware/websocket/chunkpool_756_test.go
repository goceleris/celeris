package websocket

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// patterned returns n bytes that identify (seq, offset), so a chunk recycled
// too early shows up as a content mismatch.
func patterned(seq, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(seq*131 + i*7)
	}
	return b
}

func TestGetChunkClasses756(t *testing.T) {
	for _, n := range []int{1, 100, 512, 513, 4096, 8192, 8193, 65550, 1 << 18} {
		b := getChunk(n)
		if len(b) != n {
			t.Fatalf("getChunk(%d): len %d", n, len(b))
		}
		c := cap(b)
		switch {
		case n < 512:
			// Below the smallest class: an exactly sized plain allocation,
			// never rounded up to 512 B.
			if c != n {
				t.Fatalf("getChunk(%d): cap %d, want %d (small reads are not rounded up)", n, c, n)
			}
		case c < n || c&(c-1) != 0:
			t.Fatalf("getChunk(%d): cap %d is not a power-of-two class >= n", n, c)
		}
		putChunk(b)
	}
	// Outside the classes: a plain allocation, not pooled.
	big := getChunk(1<<18 + 1)
	if len(big) != 1<<18+1 {
		t.Fatalf("oversized chunk len %d", len(big))
	}
	if getChunk(0) != nil || getChunk(-1) != nil {
		t.Fatal("non-positive n must return nil")
	}
}

// A buffer whose capacity is not an exact class size must never enter a pool:
// it could be handed out as a larger buffer than it is.
func TestPutChunkIgnoresForeignBuffers756(t *testing.T) {
	putChunk(nil)
	putChunk(make([]byte, 10))       // below the smallest class
	putChunk(make([]byte, 600))      // not a power of two
	putChunk(make([]byte, 1<<18+1))  // above the largest class
	putChunk(make([]byte, 0, 1<<20)) // above the largest class, power of two
	for i := 0; i < 64; i++ {
		b := getChunk(600)
		if cap(b) != 1024 {
			t.Fatalf("a foreign buffer leaked into a class: cap %d", cap(b))
		}
		putChunk(b)
	}
}

// AppendCopy must copy: the caller reuses its slice right after the call.
func TestAppendCopyCopies756(t *testing.T) {
	r := newChanReader(8, 0, 0)
	r.recycle = true
	src := patterned(1, 4000)
	want := append([]byte(nil), src...)
	if !r.AppendCopy(src) {
		t.Fatal("AppendCopy rejected")
	}
	for i := range src {
		src[i] = 0xEE // the engine reusing its read buffer
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("AppendCopy aliased the caller's slice")
	}
}

// Ownership: a pooled chunk must not be recycled while any byte of it is still
// unread. Read one byte at a time (the chunk is partially consumed for most of
// the test), churn the pool from other goroutines between every read, and
// check each byte.
func TestRecycleNeverBeforeFullyConsumed756(t *testing.T) {
	r := newChanReader(64, 0, 0)
	r.recycle = true
	const chunks, size = 16, 3000
	var want []byte
	for i := 0; i < chunks; i++ {
		c := patterned(i, size)
		want = append(want, c...)
		if !r.AppendCopy(c) {
			t.Fatalf("AppendCopy %d rejected", i)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b := getChunk(size)
				for i := range b {
					b[i] = 0xAA // a recycled-too-early chunk would be overwritten here
				}
				putChunk(b)
			}
		}()
	}
	// A chunk recycled early loses bytes and the loop below would wait for
	// them forever: fail the read instead of hanging the test binary.
	watchdog := time.AfterFunc(30*time.Second, func() { r.closeWith(errors.New("stream lost bytes: test watchdog")) })
	defer watchdog.Stop()
	got := make([]byte, 0, len(want))
	one := make([]byte, 1)
	for len(got) < len(want) {
		n, err := r.Read(one)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, one[:n]...)
	}
	close(stop)
	wg.Wait()
	if !bytes.Equal(got, want) {
		t.Fatal("stream corrupted: a chunk was recycled before it was fully consumed")
	}
}

// A reader that is not recycling (fed by Append with caller-owned slices) must
// never put those slices in the pool.
func TestNoRecycleForCallerOwnedChunks756(t *testing.T) {
	r := newChanReader(8, 0, 0)
	owned := patterned(2, 2048) // cap 2048: a class size, the worst case
	want := append([]byte(nil), owned...)
	if !r.Append(owned) {
		t.Fatal("Append rejected")
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		b := getChunk(2048)
		for j := range b {
			b[j] = 0xBB
		}
		putChunk(b)
	}
	if !bytes.Equal(owned, want) {
		t.Fatal("a caller-owned chunk was recycled and overwritten")
	}
}

// Producer and consumer on different goroutines, as on the engine path, with
// the spill in play (a small channel and no pauser): every byte must arrive in
// order. Run with -race.
func TestRecycleProducerConsumerRace756(t *testing.T) {
	r := newChanReader(4, 0, 0) // channel 4 + spill 4: the producer is paced below
	r.recycle = true
	const msgs = 2000
	sizes := []int{8192, 8192, 8192, 8192, 8192, 8192, 8192, 8174, 100, 513}
	var total int
	for i := 0; i < msgs; i++ {
		total += sizes[i%len(sizes)]
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < msgs; i++ {
			c := patterned(i, sizes[i%len(sizes)])
			for !r.AppendCopy(c) {
				t.Errorf("AppendCopy %d rejected", i)
				return
			}
			// Pace the producer so the 8-chunk bound holds without a pauser.
			for r.buffered() > 3 {
				runtime.Gosched()
			}
		}
	}()
	buf := make([]byte, 4096)
	read := 0
	var seq, off int
	for read < total {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < n; k++ {
			if off == sizes[seq%len(sizes)] {
				seq++
				off = 0
			}
			if want := byte(seq*131 + off*7); buf[k] != want {
				t.Fatalf("msg %d off %d: got %#x want %#x", seq, off, buf[k], want)
			}
			off++
		}
		read += n
	}
	<-done
}

// The loop-side delivery of one 65550-byte masked frame arriving in 8 KiB
// reads, as the engine hands it to the WebSocket callback, and the handler
// side draining it: the allocations the loop thread paid per message before
// celeris#756 (make+copy per chunk) against the pooled copy.
const benchFrame756 = 65550
const benchRead756 = 8192

func benchDeliver756(b *testing.B, pooled bool) {
	src := patterned(7, benchRead756)
	r := newChanReader(256, 0, 0)
	r.recycle = pooled
	sink := make([]byte, benchFrame756)
	b.ReportAllocs()
	b.SetBytes(benchFrame756)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for sent := 0; sent < benchFrame756; sent += benchRead756 {
			n := min(benchRead756, benchFrame756-sent)
			if pooled {
				r.AppendCopy(src[:n])
			} else {
				cp := make([]byte, n)
				copy(cp, src[:n])
				r.Append(cp)
			}
		}
		for got := 0; got < benchFrame756; {
			n, err := r.Read(sink[got:])
			if err != nil {
				b.Fatal(err)
			}
			got += n
		}
	}
}

func BenchmarkChunkDeliverMake756(b *testing.B)   { benchDeliver756(b, false) }
func BenchmarkChunkDeliverPooled756(b *testing.B) { benchDeliver756(b, true) }
