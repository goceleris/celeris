package stream

// State represents the state of an HTTP/2 stream as defined in RFC 7540.
type State int

const (
	StateIdle State = iota
	StateOpen
	StateHalfClosedLocal
	StateHalfClosedRemote
	StateClosed
)

var stateNames = [5]string{
	"Idle",
	"Open",
	"HalfClosedLocal",
	"HalfClosedRemote",
	"Closed",
}

func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "Unknown"
}

// Phase represents the response phase for a stream to ensure proper write ordering.
type Phase int

const (
	PhaseInit Phase = iota
	PhaseHeadersSent
	PhaseBody
)

var phaseNames = [3]string{
	"Init",
	"HeadersSent",
	"Body",
}

func (p Phase) String() string {
	if int(p) < len(phaseNames) {
		return phaseNames[p]
	}
	return "Unknown"
}
