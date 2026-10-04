package probe

// SyscallProber holds platform-specific functions for probing system capabilities.
type SyscallProber struct {
	ReadKernelVersion func() (string, error)
	ProbeIOUring      func() (features uint32, ops []uint8, err error)
	ProbeEpoll        func() bool
	CheckCapSysNice   func() bool
	ReadNUMANodes     func() int
}
