package engine

// CapabilityProfile describes the I/O capabilities detected on the current
// system. The probe package populates this at startup to guide engine selection.
type CapabilityProfile struct {
	// OS is the operating system name (e.g. "linux").
	OS string
	// KernelVersion is the full kernel version string (e.g. "5.15.0-91-generic").
	KernelVersion string
	// KernelMajor is the major kernel version number.
	KernelMajor int
	// KernelMinor is the minor kernel version number.
	KernelMinor int
	// IOUringTier is the detected io_uring capability tier (None through Optional).
	IOUringTier Tier
	// EpollAvailable is true if epoll is supported on this system.
	EpollAvailable bool
	// MultishotAccept is true if io_uring multishot accept is available (kernel 5.19+).
	MultishotAccept bool
	// MultishotRecv is true if io_uring multishot recv is available (kernel 5.19+).
	MultishotRecv bool
	// ProvidedBuffers is true if io_uring provided buffer rings are available (kernel 5.19+).
	ProvidedBuffers bool
	// SQPoll is true if io_uring SQ polling mode is available (kernel 6.0+).
	SQPoll bool
	// CoopTaskrun is true if IORING_SETUP_COOP_TASKRUN is available (kernel 5.19+).
	CoopTaskrun bool
	// SingleIssuer is true if IORING_SETUP_SINGLE_ISSUER is available (kernel 6.0+).
	SingleIssuer bool
	// LinkedSQEs is true if io_uring linked SQE chains are supported.
	LinkedSQEs bool
	// DeferTaskrun is true if IORING_SETUP_DEFER_TASKRUN is available (kernel 6.1+).
	DeferTaskrun bool
	// FixedFiles is true if io_uring fixed file descriptors are available.
	FixedFiles bool
	// SendZC is true if io_uring zero-copy send is available.
	SendZC bool
	// NumCPU is the number of logical CPUs.
	NumCPU int
	// NUMANodes is the number of NUMA nodes detected.
	NUMANodes int
}

// NewDefaultProfile returns a CapabilityProfile with safe defaults.
func NewDefaultProfile() CapabilityProfile {
	return CapabilityProfile{
		IOUringTier: None,
		NUMANodes:   1,
	}
}
