//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/cpumon"
	"github.com/goceleris/celeris/internal/engine"
)

func TestLiveSamplerNilMonitorDegradesGracefully(t *testing.T) {
	s := newLiveSampler(nil)
	if s.cpuMon != nil {
		t.Fatalf("expected nil cpuMon, got %T", s.cpuMon)
	}
	e := newMockEngine(engine.Epoll)
	snap := s.Sample(e)
	if snap.CPUUtilization != 0 {
		t.Errorf("CPUUtilization = %v, want 0 (no monitor wired)", snap.CPUUtilization)
	}
}

func TestLiveSamplerSyntheticMonitorPopulatesCPU(t *testing.T) {
	mon := cpumon.NewSynthetic(0.85)
	s := newLiveSampler(mon)
	e := newMockEngine(engine.Epoll)

	snap := s.Sample(e)
	if snap.CPUUtilization != 0.85 {
		t.Errorf("CPUUtilization = %v, want 0.85", snap.CPUUtilization)
	}

	mon.Set(0.42)
	snap = s.Sample(e)
	if snap.CPUUtilization != 0.42 {
		t.Errorf("CPUUtilization after update = %v, want 0.42", snap.CPUUtilization)
	}
}

func TestLiveSamplerDeltasThroughputAndError(t *testing.T) {
	s := newLiveSampler(nil)
	primary := newMockEngine(engine.Epoll)
	primary.SetMetrics(engine.EngineMetrics{
		RequestCount: 100,
		ErrorCount:   5,
	})

	// First sample: baseline (no deltas).
	snap := s.Sample(primary)
	if snap.ThroughputRPS != 0 {
		t.Errorf("first ThroughputRPS = %v, want 0", snap.ThroughputRPS)
	}
	if snap.ErrorRate != 0 {
		t.Errorf("first ErrorRate = %v, want 0", snap.ErrorRate)
	}

	// Advance time and accumulate counts.
	time.Sleep(110 * time.Millisecond)
	primary.SetMetrics(engine.EngineMetrics{
		RequestCount: 200, // +100 in ~110ms
		ErrorCount:   15,  // +10
	})
	snap = s.Sample(primary)

	want := 100.0 / 0.110
	if snap.ThroughputRPS < want*0.5 || snap.ThroughputRPS > want*1.5 {
		t.Errorf("ThroughputRPS = %v, expected ~%v (±50%%)", snap.ThroughputRPS, want)
	}
	if snap.ErrorRate != 0.10 {
		t.Errorf("ErrorRate = %v, want 0.10", snap.ErrorRate)
	}
}

func TestLiveSamplerPerEngineMetrics(t *testing.T) {
	s := newLiveSampler(nil)
	epollEng := newMockEngine(engine.Epoll)
	iouringEng := newMockEngine(engine.IOUring)

	epollEng.SetMetrics(engine.EngineMetrics{RequestCount: 50})
	iouringEng.SetMetrics(engine.EngineMetrics{RequestCount: 70})

	snapEpoll := s.Sample(epollEng)
	if snapEpoll.ThroughputRPS != 0 {
		t.Errorf("first epoll ThroughputRPS = %v, want 0", snapEpoll.ThroughputRPS)
	}

	snapIouring := s.Sample(iouringEng)
	if snapIouring.ThroughputRPS != 0 {
		t.Errorf("first iouring ThroughputRPS = %v, want 0 (per-engine prev map)", snapIouring.ThroughputRPS)
	}

	// Second sample for each — deltas should be tracked per engine type.
	time.Sleep(50 * time.Millisecond)
	epollEng.SetMetrics(engine.EngineMetrics{RequestCount: 100})
	snapEpoll = s.Sample(epollEng)
	if snapEpoll.ThroughputRPS <= 0 {
		t.Errorf("second epoll ThroughputRPS = %v, want > 0", snapEpoll.ThroughputRPS)
	}
}

// TestSampleCounterThatFellReadsZero886 is a neighbour of the per-bucket delta
// (celeris#886 item 10): the request, accept and byte deltas were plain
// subtractions while the error deltas saturate. A total that reads lower than
// at the last sample (nothing in the engines does this; a wiring slip could)
// gave a delta near 2^64 and a throughput of 1e19 requests per second, which
// the controller would then act on. It must read as no traffic.
func TestSampleCounterThatFellReadsZero886(t *testing.T) {
	e := newMockEngine(engine.IOUring)
	s := newLiveSampler(nil)
	e.SetMetrics(engine.EngineMetrics{RequestCount: 100, AcceptCount: 10, BytesRead: 600, BytesWritten: 400})
	s.Sample(e)
	time.Sleep(2 * time.Millisecond)
	e.SetMetrics(engine.EngineMetrics{RequestCount: 50, AcceptCount: 5, BytesRead: 300, BytesWritten: 200})
	snap := s.Sample(e)
	if snap.ThroughputRPS != 0 || snap.AcceptRate != 0 || snap.BytesPerReq != 0 || snap.ErrorRate != 0 {
		t.Errorf("counters that fell read as rps=%v accept_rate=%v bytes_per_req=%v error_rate=%v, want all 0",
			snap.ThroughputRPS, snap.AcceptRate, snap.BytesPerReq, snap.ErrorRate)
	}
}

// TestSampleByteCounterThatFellReadsZeroBytesPerReq886 guards the byte delta
// on its own. In TestSampleCounterThatFellReadsZero886 the request total falls
// too, so BytesPerReq stays 0 through the deltaReqs > 0 gate whatever the byte
// delta does. Here requests grow and only the bytes fall: a plain subtraction
// gives a delta near 2^64, a BytesPerReq near 1.8e17 (far above
// largePayloadBytes, which suppresses an io_uring switch), where the saturating
// one reads 0.
func TestSampleByteCounterThatFellReadsZeroBytesPerReq886(t *testing.T) {
	e := newMockEngine(engine.IOUring)
	s := newLiveSampler(nil)
	e.SetMetrics(engine.EngineMetrics{RequestCount: 100, BytesRead: 600, BytesWritten: 400})
	s.Sample(e)
	time.Sleep(2 * time.Millisecond)
	e.SetMetrics(engine.EngineMetrics{RequestCount: 200, BytesRead: 300, BytesWritten: 200})
	snap := s.Sample(e)
	if snap.BytesPerReq != 0 {
		t.Errorf("bytes fell while requests grew: BytesPerReq = %v, want 0", snap.BytesPerReq)
	}
	if snap.ThroughputRPS <= 0 {
		t.Errorf("ThroughputRPS = %v, want > 0 (the request counter grew)", snap.ThroughputRPS)
	}
}
