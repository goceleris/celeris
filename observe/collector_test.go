package observe

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNewCollector(t *testing.T) {
	c := NewCollector()
	if c == nil {
		t.Fatal("NewCollector returned nil")
	}
	snap := c.Snapshot()
	if snap.RequestsTotal != 0 || snap.ErrorsTotal != 0 || snap.ActiveConns != 0 || snap.EngineSwitches != 0 {
		t.Fatalf("fresh snapshot should be zero, got %+v", snap)
	}
}

func TestRecordRequest(t *testing.T) {
	c := NewCollector()
	c.RecordRequest(500*time.Microsecond, 200) // bucket 0 (<=0.001)
	c.RecordRequest(3*time.Millisecond, 200)   // bucket 1 (<=0.005)
	c.RecordRequest(50*time.Millisecond, 503)  // bucket 4 (<=0.05), also error
	c.RecordRequest(10*time.Second, 200)       // bucket 9 (overflow into last)

	snap := c.Snapshot()
	if snap.RequestsTotal != 4 {
		t.Fatalf("expected 4 requests, got %d", snap.RequestsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Fatalf("expected 1 error from 503 status, got %d", snap.ErrorsTotal)
	}
	if snap.LatencyBuckets[0] != 1 {
		t.Fatalf("expected bucket[0]=1, got %d", snap.LatencyBuckets[0])
	}
	if snap.LatencyBuckets[1] != 1 {
		t.Fatalf("expected bucket[1]=1, got %d", snap.LatencyBuckets[1])
	}
	if snap.LatencyBuckets[4] != 1 {
		t.Fatalf("expected bucket[4]=1, got %d", snap.LatencyBuckets[4])
	}
	if snap.LatencyBuckets[9] != 1 {
		t.Fatalf("expected bucket[9]=1, got %d", snap.LatencyBuckets[9])
	}
}

func TestRecordRequestServerErrors(t *testing.T) {
	c := NewCollector()
	c.RecordRequest(time.Millisecond, 500)
	c.RecordRequest(time.Millisecond, 502)
	c.RecordRequest(time.Millisecond, 499)
	snap := c.Snapshot()
	if snap.ErrorsTotal != 2 {
		t.Fatalf("expected 2 errors (500, 502), got %d", snap.ErrorsTotal)
	}
}

func TestRecordError(t *testing.T) {
	c := NewCollector()
	c.RecordError()
	c.RecordError()
	snap := c.Snapshot()
	if snap.ErrorsTotal != 2 {
		t.Fatalf("expected 2, got %d", snap.ErrorsTotal)
	}
}

func TestActiveConnsFromEngineMetrics(t *testing.T) {
	c := NewCollector()
	c.SetEngineMetricsFn(func() EngineMetrics {
		return EngineMetrics{ActiveConnections: 42}
	})
	snap := c.Snapshot()
	if snap.ActiveConns != 42 {
		t.Fatalf("expected ActiveConns=42 from EngineMetrics, got %d", snap.ActiveConns)
	}
}

func TestRecordSwitch(t *testing.T) {
	c := NewCollector()
	c.RecordSwitch()
	c.RecordSwitch()
	snap := c.Snapshot()
	if snap.EngineSwitches != 2 {
		t.Fatalf("expected 2, got %d", snap.EngineSwitches)
	}
}

func TestSnapshotWithEngineMetrics(t *testing.T) {
	c := NewCollector()
	c.SetEngineMetricsFn(func() EngineMetrics {
		return EngineMetrics{
			RequestCount:      100,
			ActiveConnections: 10,
			LatencyP50:        5 * time.Millisecond,
		}
	})
	snap := c.Snapshot()
	if snap.EngineMetrics.RequestCount != 100 {
		t.Fatalf("expected engine request count 100, got %d", snap.EngineMetrics.RequestCount)
	}
	if snap.EngineMetrics.ActiveConnections != 10 {
		t.Fatalf("expected 10 active conns, got %d", snap.EngineMetrics.ActiveConnections)
	}
	if snap.EngineMetrics.LatencyP50 != 5*time.Millisecond {
		t.Fatalf("expected 5ms p50, got %v", snap.EngineMetrics.LatencyP50)
	}
}

func TestSnapshotBucketBounds(t *testing.T) {
	c := NewCollector()
	snap := c.Snapshot()
	if len(snap.BucketBounds) != len(defaultBucketBounds) {
		t.Fatalf("expected %d bounds, got %d", len(defaultBucketBounds), len(snap.BucketBounds))
	}
	for i, b := range snap.BucketBounds {
		if b != defaultBucketBounds[i] {
			t.Fatalf("bound[%d]: expected %f, got %f", i, defaultBucketBounds[i], b)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.RecordRequest(time.Duration(n)*time.Microsecond, 200)
			c.RecordSwitch()
		}(i)
	}
	wg.Wait()
	snap := c.Snapshot()
	if snap.RequestsTotal != 100 {
		t.Fatalf("expected 100 requests, got %d", snap.RequestsTotal)
	}
	if snap.EngineSwitches != 100 {
		t.Fatalf("expected 100 switches, got %d", snap.EngineSwitches)
	}
}

func TestSnapshotCPUUtilizationDefault(t *testing.T) {
	c := NewCollector()
	snap := c.Snapshot()
	if snap.CPUUtilization != -1 {
		t.Fatalf("expected -1 without CPU monitor, got %f", snap.CPUUtilization)
	}
}

type mockCPUMonitor struct {
	util float64
	err  error
}

func (m *mockCPUMonitor) Close() error { return nil }
func (m *mockCPUMonitor) Sample() (float64, error) {
	return m.util, m.err
}

func TestSnapshotWithCPUMonitor(t *testing.T) {
	c := NewCollector()
	c.SetCPUMonitor(&mockCPUMonitor{util: 0.75})
	snap := c.Snapshot()
	if snap.CPUUtilization != 0.75 {
		t.Fatalf("expected 0.75, got %f", snap.CPUUtilization)
	}
}

func TestSnapshotCPUMonitorError(t *testing.T) {
	c := NewCollector()
	c.SetCPUMonitor(&mockCPUMonitor{util: -1, err: errors.New("sample failed")})
	snap := c.Snapshot()
	if snap.CPUUtilization != -1 {
		t.Fatalf("expected -1 on error, got %f", snap.CPUUtilization)
	}
}

func TestNewCPUMonitor(t *testing.T) {
	m, err := NewCPUMonitor()
	if err != nil {
		t.Fatalf("NewCPUMonitor: %v", err)
	}
	util, err := m.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if util < 0 || util > 1 {
		t.Fatalf("expected utilization in [0,1], got %f", util)
	}
}
