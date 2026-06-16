//go:build linux

package adaptive

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/cpumon"
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
