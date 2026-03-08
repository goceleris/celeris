package engine

// CapabilityProfile describes the capabilities detected on the current system.
type CapabilityProfile struct {
	OS              string
	KernelVersion   string
	KernelMajor     int
	KernelMinor     int
	IOUringTier     Tier
	EpollAvailable  bool
	MultishotAccept bool
	MultishotRecv   bool
	ProvidedBuffers bool
	SQPoll          bool
	CoopTaskrun     bool
	SingleIssuer    bool
	LinkedSQEs      bool
	NumCPU          int
	NUMANodes       int
}

// NewDefaultProfile returns a CapabilityProfile with safe defaults.
func NewDefaultProfile() CapabilityProfile {
	return CapabilityProfile{
		IOUringTier: None,
		NUMANodes:   1,
	}
}
