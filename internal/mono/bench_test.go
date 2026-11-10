package mono

import (
	"testing"
	"time"
)

var (
	sinkInt  int64
	sinkTime time.Time
)

// The hot paths: a clock read per CQE batch or events return, and per request
// on the HTTP/1 keep-alive path (the header deadline). "Before" is today's
// time.Now().UnixNano().

func BenchmarkBeforeWallUnixNano(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sinkInt = time.Now().UnixNano()
	}
}

// BenchmarkNowRouting is mono.Now() as this PR ships it: the seam's atomic load
// and the wall read. What the routing PRs add to every read until the flip.
func BenchmarkNowRouting(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sinkInt = Now()
	}
}

// BenchmarkNowFlipped is the form after the flip (the seam's load and one
// time.Since), written out because flipped is a constant.
func BenchmarkNowFlipped(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		if seam.Load() != nil {
			sinkInt = -1
		}
		sinkInt = nowMonotonic()
	}
}

func BenchmarkNowFlippedNoSeam(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		sinkInt = nowMonotonic()
	}
}

func BenchmarkFromWall(b *testing.B) {
	b.ReportAllocs()
	w := time.Now().UnixNano() + 5
	for range b.N {
		sinkInt = FromWall(w)
	}
}

// BenchmarkStartTimeToday is Context.StartTime() as it is: time.Unix(0, ns).
func BenchmarkStartTimeToday(b *testing.B) {
	b.ReportAllocs()
	ns := Now()
	for range b.N {
		sinkTime = time.Unix(0, ns)
	}
}

func BenchmarkTime(b *testing.B) {
	b.ReportAllocs()
	ns := Now()
	for range b.N {
		sinkTime = Time(ns)
	}
}

// The handler.go fallback, taken by every HTTP/2 request and every std-engine
// request: it already holds a time.Now().

func BenchmarkFallbackToday(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		t := time.Now()
		sinkTime, sinkInt = t, t.UnixNano()
	}
}

func BenchmarkFallbackWithASecondNow(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		t := time.Now()
		sinkTime, sinkInt = t, Now()
	}
}

func BenchmarkFallbackFromTime(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		t := time.Now()
		sinkTime, sinkInt = t, FromTime(t)
	}
}

func BenchmarkFallbackFromTimeFlipped(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		t := time.Now()
		sinkTime = t
		if seam.Load() != nil {
			sinkInt = -1
		}
		sinkInt = fromTimeMonotonic(t)
	}
}

func BenchmarkParallelBeforeWall(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var s int64
		for pb.Next() {
			s = time.Now().UnixNano()
		}
		_ = s
	})
}

func BenchmarkParallelNowFlipped(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var s int64
		for pb.Next() {
			if seam.Load() != nil {
				s++
			}
			s += nowMonotonic()
		}
		_ = s
	})
}
