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
		sq_entries: 1,
		flags:      0,
	}
	fd, err := ioUringSetup(1, params)
	if err != nil {
		return 0, nil, err
	}
	defer unix.Close(fd)

	features := params.features

	ops, _ := ioUringProbeOps(fd)

	return features, ops, nil
}

type ioUringParams struct {
	sq_entries     uint32
	cq_entries     uint32
	flags          uint32
	sq_thread_cpu  uint32
	sq_thread_idle uint32
	features       uint32
	wq_fd          uint32
	resv           [3]uint32
	sq_off         [10]uint32
	cq_off         [10]uint32
}

func ioUringSetup(entries uint32, params *ioUringParams) (int, error) {
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func ioUringProbeOps(fd int) ([]uint8, error) {
	const probeSize = 256 + 8
	buf := make([]byte, probeSize*8)
	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(fd), 8, uintptr(unsafe.Pointer(&buf[0])), 256, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return nil, nil
}

func probeEpollLinux() bool {
	fd, err := unix.EpollCreate1(0)
	if err != nil {
		return false
	}
	unix.Close(fd)
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
