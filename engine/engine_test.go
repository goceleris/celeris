package engine

import (
	"context"
	"net"
	"testing"
	"time"
)

type mockEngine struct{}

func (m *mockEngine) Listen(_ context.Context) error   { return nil }
func (m *mockEngine) Shutdown(_ context.Context) error { return nil }
func (m *mockEngine) Metrics() EngineMetrics           { return EngineMetrics{} }
func (m *mockEngine) Type() EngineType                 { return Std }
func (m *mockEngine) Addr() net.Addr                   { return nil }

var _ Engine = (*mockEngine)(nil)

func TestEngineMetricsZeroValue(t *testing.T) {
	var m EngineMetrics
	if m.RequestCount != 0 {
		t.Errorf("RequestCount = %d, want 0", m.RequestCount)
	}
	if m.ActiveConnections != 0 {
		t.Errorf("ActiveConnections = %d, want 0", m.ActiveConnections)
	}
	if m.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0", m.ErrorCount)
	}
	if m.Throughput != 0 {
		t.Errorf("Throughput = %f, want 0", m.Throughput)
	}
	if m.LatencyP50 != 0 {
		t.Errorf("LatencyP50 = %v, want 0", m.LatencyP50)
	}
	if m.LatencyP99 != 0 {
		t.Errorf("LatencyP99 = %v, want 0", m.LatencyP99)
	}
	if m.LatencyP999 != 0 {
		t.Errorf("LatencyP999 = %v, want 0", m.LatencyP999)
	}
}

func TestMockEngineMetricsReturnsZero(t *testing.T) {
	var e mockEngine
	m := e.Metrics()
	if m.RequestCount != 0 || m.ActiveConnections != 0 || m.ErrorCount != 0 {
		t.Error("mock Metrics() should return zero-value EngineMetrics")
	}
}

func TestMockEngineType(t *testing.T) {
	var e mockEngine
	if e.Type() != Std {
		t.Errorf("Type() = %v, want %v", e.Type(), Std)
	}
}

func TestEngineMetricsFieldValues(t *testing.T) {
	m := EngineMetrics{
		RequestCount:      1000,
		ActiveConnections: 50,
		ErrorCount:        3,
		Throughput:        12345.67,
		LatencyP50:        500 * time.Microsecond,
		LatencyP99:        5 * time.Millisecond,
		LatencyP999:       50 * time.Millisecond,
	}
	if m.RequestCount != 1000 {
		t.Errorf("RequestCount = %d, want 1000", m.RequestCount)
	}
	if m.ActiveConnections != 50 {
		t.Errorf("ActiveConnections = %d, want 50", m.ActiveConnections)
	}
	if m.ErrorCount != 3 {
		t.Errorf("ErrorCount = %d, want 3", m.ErrorCount)
	}
	if m.Throughput != 12345.67 {
		t.Errorf("Throughput = %f, want 12345.67", m.Throughput)
	}
	if m.LatencyP50 != 500*time.Microsecond {
		t.Errorf("LatencyP50 = %v, want 500us", m.LatencyP50)
	}
	if m.LatencyP99 != 5*time.Millisecond {
		t.Errorf("LatencyP99 = %v, want 5ms", m.LatencyP99)
	}
	if m.LatencyP999 != 50*time.Millisecond {
		t.Errorf("LatencyP999 = %v, want 50ms", m.LatencyP999)
	}
}
