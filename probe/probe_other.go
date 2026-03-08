//go:build !linux

package probe

func defaultProber() *SyscallProber {
	return &SyscallProber{
		ReadKernelVersion: func() (string, error) { return "", nil },
		ProbeIOUring:      nil,
		ProbeEpoll:        nil,
		CheckCapSysNice:   nil,
		ReadNUMANodes:     func() int { return 1 },
	}
}
