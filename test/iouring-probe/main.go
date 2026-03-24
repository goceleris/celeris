//go:build linux

// Minimal io_uring probe that tests every flag combination to find what works.
// Run with: sudo ./iouring-probe
package main

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sysIOUringSetup = 425

	setupSQPoll       = 1 << 1
	setupSQAff        = 1 << 2
	setupCoopTaskrun  = 1 << 8
	setupSingleIssuer = 1 << 12
	setupDeferTaskrun = 1 << 13
)

type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFD         uint32
	resv         [3]uint32
	sqOff        [10]uint32 // simplified
	cqOff        [10]uint32 // simplified
}

func trySetup(label string, entries uint32, flags uint32, sqIdle uint32, sqCPU int) {
	var params ioUringParams
	params.flags = flags
	params.sqThreadIdle = sqIdle
	if sqCPU >= 0 {
		params.flags |= setupSQAff
		params.sqThreadCPU = uint32(sqCPU)
	}

	fd, _, errno := unix.RawSyscall(
		uintptr(sysIOUringSetup),
		uintptr(entries),
		uintptr(unsafe.Pointer(&params)),
		0,
	)
	if errno != 0 {
		fmt.Printf("  %-50s  FAIL: %v\n", label, errno)
	} else {
		fmt.Printf("  %-50s  OK (fd=%d, features=0x%x)\n", label, fd, params.features)
		_ = unix.Close(int(fd))
	}
}

func main() {
	fmt.Printf("io_uring SQPOLL probe\n")
	fmt.Printf("Kernel: ")
	if data, err := os.ReadFile("/proc/version"); err == nil {
		fmt.Printf("%s", data)
	}
	fmt.Printf("UID: %d, EUID: %d\n", os.Getuid(), os.Geteuid())
	fmt.Printf("NumCPU: %d\n", runtime.NumCPU())

	// Check AppArmor
	if data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_io_uring"); err == nil {
		fmt.Printf("AppArmor io_uring restriction: %s", data)
	} else {
		fmt.Printf("AppArmor io_uring restriction: not present (%v)\n", err)
	}
	if data, err := os.ReadFile("/proc/sys/kernel/io_uring_disabled"); err == nil {
		fmt.Printf("io_uring_disabled: %s", data)
	} else {
		fmt.Printf("io_uring_disabled: not present\n")
	}

	fmt.Println("\n--- Basic ring creation ---")
	trySetup("bare (no flags)", 256, 0, 0, -1)
	trySetup("COOP_TASKRUN", 256, setupCoopTaskrun, 0, -1)
	trySetup("SINGLE_ISSUER", 256, setupSingleIssuer, 0, -1)
	trySetup("COOP+SINGLE", 256, setupCoopTaskrun|setupSingleIssuer, 0, -1)
	trySetup("DEFER_TASKRUN+SINGLE", 256, setupDeferTaskrun|setupSingleIssuer, 0, -1)

	fmt.Println("\n--- SQPOLL variants ---")
	trySetup("SQPOLL only", 256, setupSQPoll, 2000, -1)
	trySetup("SQPOLL idle=0", 256, setupSQPoll, 0, -1)
	trySetup("SQPOLL idle=5000", 256, setupSQPoll, 5000, -1)
	trySetup("SQPOLL+SQ_AFF cpu=0", 256, setupSQPoll, 2000, 0)
	trySetup("SQPOLL+SQ_AFF cpu=1", 256, setupSQPoll, 2000, 1)
	trySetup("SQPOLL+COOP", 256, setupCoopTaskrun|setupSQPoll, 2000, -1)
	trySetup("SQPOLL+SINGLE", 256, setupSingleIssuer|setupSQPoll, 2000, -1)
	trySetup("SQPOLL+COOP+SINGLE", 256, setupCoopTaskrun|setupSingleIssuer|setupSQPoll, 2000, -1)

	fmt.Println("\n--- SQPOLL with different ring sizes ---")
	for _, size := range []uint32{32, 128, 256, 1024, 4096, 16384, 32768} {
		trySetup(fmt.Sprintf("SQPOLL entries=%d", size), size, setupSQPoll, 2000, -1)
	}

	fmt.Println("\n--- Our actual configs ---")
	// What the optional tier was doing (old, broken):
	trySetup("optionalTier OLD (COOP+SINGLE+SQPOLL)", 16384, setupCoopTaskrun|setupSingleIssuer|setupSQPoll, 2000, -1)
	// What we changed it to:
	trySetup("optionalTier NEW (SQPOLL only)", 16384, setupSQPoll, 2000, -1)
	// With SQ_AFF (what NewRingCPU adds):
	trySetup("optionalTier+SQ_AFF cpu=0", 16384, setupSQPoll, 2000, 0)
	// The high tier (fallback):
	trySetup("highTier (DEFER+SINGLE)", 16384, setupDeferTaskrun|setupSingleIssuer, 0, -1)
	trySetup("highTier (COOP+SINGLE)", 16384, setupCoopTaskrun|setupSingleIssuer, 0, -1)
}
