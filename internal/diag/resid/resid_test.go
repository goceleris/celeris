package resid

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestBucketing(t *testing.T) {
	// Every bucket's lower bound is in it, the value before the next bound is
	// too, and the bounds grow strictly: 4 linear buckets, then 4 per octave.
	prev := int64(-1)
	for i := 0; i < NumBuckets; i++ {
		lo := BucketLo(i)
		if lo <= prev {
			t.Fatalf("BucketLo(%d)=%d not above BucketLo(%d)=%d", i, lo, i-1, prev)
		}
		prev = lo
		if got := BucketOf(lo); got != i {
			t.Errorf("BucketOf(BucketLo(%d)=%d) = %d", i, lo, got)
		}
		if i+1 < NumBuckets {
			if got := BucketOf(BucketLo(i+1) - 1); got != i {
				t.Errorf("BucketOf(%d) = %d, want %d (top of bucket)", BucketLo(i+1)-1, got, i)
			}
		}
	}
	// 4 sub-buckets per octave: [8,10) [10,12) [12,14) [14,16).
	for v, want := range map[int64]int{0: 0, 3: 3, 4: 4, 7: 7, 8: 8, 9: 8, 10: 9, 11: 9, 12: 10, 14: 11, 15: 11, 16: 12, 1000: 4*8 + 3} {
		if got := BucketOf(v); got != want {
			t.Errorf("BucketOf(%d) = %d, want %d", v, got, want)
		}
	}
}

func TestBucketOverflowAndNegative(t *testing.T) {
	last := NumBuckets - 1
	for _, v := range []int64{BucketLo(last), BucketLo(last) + 1, 1 << 50, 1 << 62, math.MaxInt64} {
		if got := BucketOf(v); got != last {
			t.Errorf("BucketOf(%d) = %d, want the overflow bucket %d", v, got, last)
		}
	}
	if got := BucketOf(BucketLo(last) - 1); got != last-1 {
		t.Errorf("just below the overflow bound: bucket %d, want %d", got, last-1)
	}
	for _, v := range []int64{-1, -1 << 40, math.MinInt64} {
		if got := BucketOf(v); got != 0 {
			t.Errorf("BucketOf(%d) = %d, want 0", v, got)
		}
	}
	var h Hist
	h.Add(-5, 3) // a negative is a zero
	h.Add(math.MaxInt64, 2)
	s := h.snap()
	if s.B[0] != 3 || s.B[last] != 2 || s.N != 5 {
		t.Errorf("negative and overflow samples: B[0]=%d B[last]=%d N=%d", s.B[0], s.B[last], s.N)
	}
}

func TestHistWeightAndSum(t *testing.T) {
	var h Hist
	h.Add(1000, 4)
	h.Add(1001, 1)
	h.Add(50_000, 2)
	s := h.snap()
	if s.N != 7 || s.SumNS != 4000+1001+100_000 {
		t.Errorf("N=%d sum=%d", s.N, s.SumNS)
	}
	if s.B[BucketOf(1000)] != 5 || s.B[BucketOf(50_000)] != 2 {
		t.Errorf("buckets %d %d", s.B[BucketOf(1000)], s.B[BucketOf(50_000)])
	}
}

// TestBucketGolden pins the bucket scheme against testdata/bucket.golden, the
// file the probatorium reader's Python is to check against (one line per
// probe: value_ns bucket lower_bound_ns).
func TestBucketGolden(t *testing.T) {
	probes := []int64{0, 1, 3, 4, 5, 7, 8, 9, 15, 16, 100, 999, 1000, 1023, 1024, 65_535, 200_000, 1_000_000, 4_294_967_296, 1 << 40, 1 << 41, math.MaxInt64}
	var b strings.Builder
	for _, v := range probes {
		i := BucketOf(v)
		fmt.Fprintf(&b, "%d %d %d\n", v, i, BucketLo(i))
	}
	want, err := os.ReadFile("testdata/bucket.golden")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != b.String() {
		t.Errorf("bucket scheme changed.\ngot:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestSamplingRate(t *testing.T) {
	l := NewLoopShift(0, SampleShift)
	const draws = 1 << 22
	n := 0
	for i := 0; i < draws; i++ {
		if l.SampleRecv() {
			n++
		}
	}
	want := draws >> SampleShift // 65536
	if n < want*95/100 || n > want*105/100 {
		t.Errorf("sampled %d of %d, want about %d (1 in %d)", n, draws, want, 1<<SampleShift)
	}
	// The turn sampler is another stream at the same rate.
	m := 0
	for i := 0; i < draws; i++ {
		if l.SampleTurn() {
			m++
		}
	}
	if m < want*95/100 || m > want*105/100 {
		t.Errorf("sampled %d turns of %d, want about %d", m, draws, want)
	}
}

// TestSamplingNoAliasing: a loop that serves two conns, reads alternating,
// samples both; a plain 1-in-64 counter would sample one of them only.
func TestSamplingNoAliasing(t *testing.T) {
	l := NewLoopShift(1, SampleShift)
	var got [2]int
	for i := 0; i < 1<<20; i++ {
		if l.SampleRecv() {
			got[i&1]++
		}
	}
	for c, n := range got {
		if n < (1<<20>>SampleShift)/2*8/10 {
			t.Errorf("conn %d sampled %d times: %v", c, n, got)
		}
	}
}

func TestSamplingEveryRecvAtShift0(t *testing.T) {
	l := NewLoopShift(2, 0)
	for i := 0; i < 1000; i++ {
		if !l.SampleRecv() {
			t.Fatal("shift 0 must sample every recv")
		}
	}
}

func TestLoopsHaveDistinctStreams(t *testing.T) {
	a, b := NewLoopShift(3, 2), NewLoopShift(4, 2)
	same := 0
	for i := 0; i < 4096; i++ {
		if a.SampleRecv() == b.SampleRecv() {
			same++
		}
	}
	if same == 4096 {
		t.Error("two loops draw the identical sampling sequence")
	}
	if a.rng == 0 || a.turnRng == 0 {
		t.Error("a zero xorshift state never leaves zero")
	}
}

func TestGateOff(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "OFF": false, " no ": false, "1": true, "true": true, "yes": true, "2": true} {
		if got := gateOn(v); got != want {
			t.Errorf("gateOn(%q) = %v, want %v", v, got, want)
		}
	}
	defer SetEnabled(false)()
	Reset()
	if Enabled() {
		t.Fatal("SetEnabled(false) left the gate on")
	}
	if l := NewLoop(0); l != nil {
		t.Fatal("NewLoop with the gate off must return nil: the engine's sites are then nil checks")
	}
	s := Snapshot()
	if s.N != 0 || len(s.Loops) != 0 || s.H[HistR].N != 0 {
		t.Errorf("a gate-off snapshot is not empty: %+v", s.N)
	}
	restore := SetEnabled(true)
	if l := NewLoop(0); l == nil {
		t.Fatal("gate on: NewLoop returned nil")
	}
	restore()
	if Enabled() {
		t.Error("restore did not turn the gate off again")
	}
	Reset()
}

func fillSnap() Snap {
	var s Snap
	s.N, s.Unst, s.Part, s.NoWr, s.Early, s.InProc = 12345, 6, 7, 8, 9, 10
	s.Loops = []uint64{1000, 0, 11345}
	for i := range s.H {
		h := &s.H[i]
		for j := 0; j < NumBuckets; j += 7 + i {
			c := uint64(j*3 + i + 1)
			h.B[j] = c
			h.N += c
		}
		h.SumNS = h.N*1234 + uint64(i)
	}
	return s
}

func TestLineRoundTrip(t *testing.T) {
	for name, s := range map[string]Snap{"full": fillSnap(), "empty": {}} {
		line := s.Line(1794317483)
		if strings.Contains(line, "\n") || !strings.HasPrefix(line, "[res] 1794317483 n=") {
			t.Fatalf("%s: bad line %q", name, line)
		}
		unix, got, err := Parse("2026/11/10 14:31:23 " + line) // the log prefix is skipped
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, line)
		}
		if unix != 1794317483 {
			t.Errorf("%s: unix %d", name, unix)
		}
		if again := got.Line(unix); again != line {
			t.Errorf("%s: round trip differs:\n%s\n%s", name, line, again)
		}
		if got.N != s.N || got.InProc != s.InProc || len(got.Loops) != len(s.Loops) || got.H != s.H {
			t.Errorf("%s: parsed snapshot differs", name)
		}
	}
}

func TestLineGolden(t *testing.T) {
	var s Snap
	s.N, s.Loops = 3, []uint64{2, 1}
	s.H[HistR] = HistSnap{N: 3, SumNS: 2300}
	s.H[HistR].B[BucketOf(100)] = 2
	s.H[HistR].B[BucketOf(2000)] = 1
	want := "[res] 5 n=3 unst=0 part=0 nowr=0 early=0 inproc=0 loops=2,1 " +
		fmt.Sprintf("R_n=3 R_sum_ns=2300 R=%d:2,%d:1 ", BucketLo(BucketOf(100)), BucketLo(BucketOf(2000))) +
		"G_n=0 G_sum_ns=0 G=- S1_n=0 S1_sum_ns=0 S1=- S2_n=0 S2_sum_ns=0 S2=- S3_n=0 S3_sum_ns=0 S3=- B_n=0 B_sum_ns=0 B=- Wt_n=0 Wt_sum_ns=0 Wt=-"
	if got := s.Line(5); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	good := fillSnap().Line(7)
	bad := map[string]string{
		"no tag":          "n=1 unst=0",
		"not a number":    strings.Replace(good, "[res] 7", "[res] x", 1),
		"missing field":   strings.Replace(good, " nowr=8", "", 1),
		"count mismatch":  strings.Replace(good, "R_n=", "R_n=1", 1),
		"not a bucket lo": strings.Replace(good, " R=0:", " R=5000001:", 1),
		"no equals":       good + " junk",
		"empty":           "[res] ",
	}
	for name, l := range bad {
		if _, _, err := Parse(l); err == nil {
			t.Errorf("%s: parsed without error: %.80q", name, l)
		}
	}
}

func TestKernelRxNS(t *testing.T) {
	if len(binary.NativeEndian.AppendUint64(nil, 0)) != 8 || ^uint(0)>>32 == 0 {
		t.Skip("64-bit ABI only")
	}
	cmsg := func(level, typ int32, data []byte) []byte {
		l := 16 + len(data)
		b := binary.NativeEndian.AppendUint64(nil, uint64(l))
		b = binary.NativeEndian.AppendUint32(b, uint32(level))
		b = binary.NativeEndian.AppendUint32(b, uint32(typ))
		b = append(b, data...)
		for len(b)%8 != 0 {
			b = append(b, 0)
		}
		return b
	}
	ts := binary.NativeEndian.AppendUint64(nil, 1794317483)
	ts = binary.NativeEndian.AppendUint64(ts, 123456789)
	other := cmsg(1, 29, []byte{1, 2, 3, 4}) // some other message first, padded
	both := append(append([]byte{}, other...), cmsg(1, 35, ts)...)
	if got := KernelRxNS(cmsg(1, 35, ts), 1, 35); got != 1794317483*1e9+123456789 {
		t.Errorf("single: %d", got)
	}
	if got := KernelRxNS(both, 1, 35); got != 1794317483*1e9+123456789 {
		t.Errorf("after another message: %d", got)
	}
	if got := KernelRxNS(other, 1, 35); got != 0 {
		t.Errorf("no timestamp message: %d", got)
	}
	if got := KernelRxNS(nil, 1, 35); got != 0 {
		t.Errorf("empty: %d", got)
	}
	trunc := cmsg(1, 35, ts)
	if got := KernelRxNS(trunc[:20], 1, 35); got != 0 {
		t.Errorf("truncated: %d", got)
	}
	bad := cmsg(1, 35, ts)
	binary.NativeEndian.PutUint64(bad, 3) // length below the header
	if got := KernelRxNS(bad, 1, 35); got != 0 {
		t.Errorf("bad length: %d", got)
	}
}

// TestConcurrentSnapshot: one writer per loop, the 1 Hz reader on another
// goroutine; run it under -race. Every snapshot's counts are the bucket sums by
// construction and never decrease (they are cumulative).
func TestConcurrentSnapshot(t *testing.T) {
	Reset()
	defer Reset()
	const loops, per = 4, 20000
	var ls [loops]*Loop
	for i := range ls {
		ls[i] = NewLoopShift(i, 0)
	}
	var wg sync.WaitGroup
	for i := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := ls[i]
			for j := 0; j < per; j++ {
				l.H[HistR].Add(int64(j%5000), 2)
				l.N.Add(1)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	var prev uint64
	for stop := false; !stop; {
		select {
		case <-done:
			stop = true
		default:
		}
		s := Snapshot()
		var sum uint64
		for _, c := range s.H[HistR].B {
			sum += c
		}
		if sum != s.H[HistR].N {
			t.Fatalf("N=%d, buckets sum to %d", s.H[HistR].N, sum)
		}
		if s.H[HistR].N < prev {
			t.Fatalf("a cumulative count went down: %d -> %d", prev, s.H[HistR].N)
		}
		prev = s.H[HistR].N
		if _, _, err := Parse(s.Line(1)); err != nil {
			t.Fatal(err)
		}
	}
	s := Snapshot()
	if s.N != loops*per || s.H[HistR].N != 2*loops*per {
		t.Errorf("final N=%d R_n=%d", s.N, s.H[HistR].N)
	}
	for i, n := range s.Loops {
		if n != per {
			t.Errorf("loop %d: %d samples, want %d", i, n, per)
		}
	}
}
