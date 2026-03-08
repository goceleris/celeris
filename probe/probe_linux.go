//go:build linux

package probe

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func defaultProber() *SyscallProber {
	return &SyscallProber{
		ReadKernelVersion: readKernelVersionLinux,
		ProbeIOUring:      probeIOUringLinux,
		ProbeEpoll:        probeEpollLinux,
		CheckCapSysNice:   checkCapSysNiceLinux,
		ReadNUMANodes:     readNUMANodesLinux,
	}
}

func readKernelVersionLinux() (string, error) {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		data, err := os.ReadFile("/proc/version")
		if err != nil {
			return "", err
		}
		fields := strings.Fields(string(data))
		if len(fields) >= 3 {
			return fields[2], nil
		}
		return "", fmt.Errorf("cannot parse /proc/version")
	}
	release := unix.ByteSliceToString(uname.Release[:])
	return release, nil
}

func probeIOUringLinux() (uint32, []uint8, error) {
	params := &ioUringParams{
		sqEntries: 1,
		flags:     0,
	}
	fd, err := ioUringSetup(1, params)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = unix.Close(fd) }()

	return params.features, nil, nil
}

// ioUringParams mirrors the kernel io_uring_params struct layout.
// Field order and sizes must match exactly for the syscall interface.
type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	resv         [3]uint32
	sqOff        [10]uint32
	cqOff        [10]uint32
}

func ioUringSetup(entries uint32, params *ioUringParams) (int, error) {
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func probeEpollLinux() bool {
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

func checkCapSysNiceLinux() bool {
	err := unix.Setpriority(unix.PRIO_PROCESS, 0, -1)
	if err == nil {
		_ = unix.Setpriority(unix.PRIO_PROCESS, 0, 0)
		return true
	}
	return false
}

func readNUMANodesLinux() int {
	entries, err := os.ReadDir("/sys/devices/system/node")
	if err != nil {
		return 1
	}
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "node") {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}
