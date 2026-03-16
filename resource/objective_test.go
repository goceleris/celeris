package resource

import "testing"

func TestObjectiveProfileString(t *testing.T) {
	tests := []struct {
		profile ObjectiveProfile
		want    string
	}{
		{BalancedObjective, "balanced"},
		{LatencyOptimized, "latency"},
		{ThroughputOptimized, "throughput"},
		{ObjectiveProfile(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.profile.String(); got != tt.want {
			t.Errorf("ObjectiveProfile(%d).String() = %q, want %q", tt.profile, got, tt.want)
		}
	}
}

func TestResolveObjectiveLatency(t *testing.T) {
	p := ResolveObjective(LatencyOptimized)
	if p.EpollTimeout != 0 {
		t.Errorf("EpollTimeout = %v, want 0 (intentional for latency)", p.EpollTimeout)
	}
	if p.SOBusyPoll == 0 {
		t.Error("SOBusyPoll should be non-zero for latency")
	}
	if p.BufferSize != 16384 {
		t.Errorf("BufferSize = %d, want 16384", p.BufferSize)
	}
	if p.SQERingScale != 1 {
		t.Errorf("SQERingScale = %d, want 1", p.SQERingScale)
	}
	if p.SQPollIdle == 0 {
		t.Error("SQPollIdle should be non-zero")
	}
	if !p.TCPNoDelay {
		t.Error("TCPNoDelay should be true")
	}
	if !p.TCPQuickAck {
		t.Error("TCPQuickAck should be true for latency")
	}
}

func TestResolveObjectiveThroughput(t *testing.T) {
	p := ResolveObjective(ThroughputOptimized)
	if p.EpollTimeout == 0 {
		t.Error("EpollTimeout should be non-zero for throughput")
	}
	if p.SOBusyPoll != 0 {
		t.Errorf("SOBusyPoll = %v, want 0 (intentional for throughput)", p.SOBusyPoll)
	}
	if p.BufferSize != 65536 {
		t.Errorf("BufferSize = %d, want 65536", p.BufferSize)
	}
	if p.SQERingScale != 2 {
		t.Errorf("SQERingScale = %d, want 2", p.SQERingScale)
	}
	if p.SQPollIdle == 0 {
		t.Error("SQPollIdle should be non-zero")
	}
	if !p.TCPNoDelay {
		t.Error("TCPNoDelay should be true")
	}
	if p.TCPQuickAck {
		t.Error("TCPQuickAck should be false for throughput")
	}
}

func TestResolveObjectiveBalanced(t *testing.T) {
	p := ResolveObjective(BalancedObjective)
	if p.EpollTimeout == 0 {
		t.Error("EpollTimeout should be non-zero for balanced")
	}
	if p.SOBusyPoll == 0 {
		t.Error("SOBusyPoll should be non-zero for balanced")
	}
	if p.BufferSize != 65536 {
		t.Errorf("BufferSize = %d, want 65536", p.BufferSize)
	}
	if p.SQERingScale != 1 {
		t.Errorf("SQERingScale = %d, want 1", p.SQERingScale)
	}
	if p.SQPollIdle == 0 {
		t.Error("SQPollIdle should be non-zero")
	}
	if !p.TCPNoDelay {
		t.Error("TCPNoDelay should be true")
	}
	if !p.TCPQuickAck {
		t.Error("TCPQuickAck should be true for balanced")
	}
}

func TestResolveObjectiveUnknownDefaultsToBalanced(t *testing.T) {
	p := ResolveObjective(ObjectiveProfile(255))
	b := ResolveObjective(BalancedObjective)
	if p != b {
		t.Errorf("unknown profile should resolve to balanced defaults")
	}
}
