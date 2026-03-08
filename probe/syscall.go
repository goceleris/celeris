package probe

type SyscallProber struct {
	ReadKernelVersion func() (string, error)
	ProbeIOUring      func() (features uint32, ops []uint8, err error)
	ProbeEpoll        func() bool
	CheckCapSysNice   func() bool
	ReadNUMANodes     func() int
}
