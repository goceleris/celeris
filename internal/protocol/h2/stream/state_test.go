package stream

import "testing"

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateIdle, "Idle"},
		{StateOpen, "Open"},
		{StateHalfClosedLocal, "HalfClosedLocal"},
		{StateHalfClosedRemote, "HalfClosedRemote"},
		{StateClosed, "Closed"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestStateStringUnknown(t *testing.T) {
	unknown := State(99)
	if got := unknown.String(); got != "Unknown" {
		t.Errorf("State(99).String() = %q, want %q", got, "Unknown")
	}
}

func TestPhaseString(t *testing.T) {
	tests := []struct {
		phase Phase
		want  string
	}{
		{PhaseInit, "Init"},
		{PhaseHeadersSent, "HeadersSent"},
		{PhaseBody, "Body"},
	}

	for _, tt := range tests {
		if got := tt.phase.String(); got != tt.want {
			t.Errorf("Phase(%d).String() = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestPhaseStringUnknown(t *testing.T) {
	unknown := Phase(99)
	if got := unknown.String(); got != "Unknown" {
		t.Errorf("Phase(99).String() = %q, want %q", got, "Unknown")
	}
}

func TestStateTransitions(t *testing.T) {
	s := NewStream(1)

	if s.GetState() != StateIdle {
		t.Errorf("Initial state: got %v, want Idle", s.GetState())
	}

	s.SetState(StateOpen)
	if s.GetState() != StateOpen {
		t.Errorf("After SetState(Open): got %v, want Open", s.GetState())
	}

	s.SetState(StateHalfClosedRemote)
	if s.GetState() != StateHalfClosedRemote {
		t.Errorf("After SetState(HalfClosedRemote): got %v, want HalfClosedRemote", s.GetState())
	}

	s.SetState(StateClosed)
	if s.GetState() != StateClosed {
		t.Errorf("After SetState(Closed): got %v, want Closed", s.GetState())
	}
}

func TestPhaseTransitions(t *testing.T) {
	s := NewStream(1)

	if s.GetPhase() != PhaseInit {
		t.Errorf("Initial phase: got %v, want Init", s.GetPhase())
	}

	s.SetPhase(PhaseHeadersSent)
	if s.GetPhase() != PhaseHeadersSent {
		t.Errorf("After SetPhase(HeadersSent): got %v, want HeadersSent", s.GetPhase())
	}

	s.SetPhase(PhaseBody)
	if s.GetPhase() != PhaseBody {
		t.Errorf("After SetPhase(Body): got %v, want Body", s.GetPhase())
	}
}
