//go:build linux

package platform

import (
	"errors"
	"io"
	"io/fs"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Reading the NICs behind a listener (celeris#1036 item 1). The rx-port rule
// (cpuplan_rxfree_linux.go) needs, once, when the engine starts:
//
//	listen address -> interfaces -> physical devices -> IRQ numbers -> CPU per IRQ
//	 (net, pure Go)   (lower_* links)  (msi_irqs, irq,    (effective_affinity_list,
//	                                    /proc/interrupts)  smp_affinity_list)
//
// plus statistics/rx_packets of each physical device. It is the topology half
// of the detection written for celeris#1062 (nicirq_linux.go), adapted: the 5%
// share test and the "dominant CPU by counts" fallback are gone from this
// side, and the packet counter read is new. nicirq_linux.go shares these
// helpers for the interrupt Warn, which leaves out the CPUs this rule frees.
//
// Everything is read from /sys/class/net and /proc, world-readable, through
// the nicFS seam, and every failure (a missing file, a permission error, a
// format the parsers do not know, a container that shows no host interface)
// makes the caller give up: the loops are then #1049's. Nothing here panics or
// returns an error to Listen.

// Bounds on what one read pass takes.
const (
	nicMaxFile       = 4 << 20 // bytes read from any one file
	nicMaxIRQsPerDev = 1024    // vectors taken from one device
	nicMaxVectors    = 4096    // vectors judged in all
	nicMaxIfaces     = 64      // interfaces a wildcard listener considers
	nicMaxDepth      = 4       // lower_* nesting followed (vlan -> bond -> slave is 2)
	nicMaxDirEntries = 4096    // entries read from one directory
	nicMaxCPUs       = 1 << 16 // CPU id accepted from a file
	nicMaxIRQ        = 1 << 24 // IRQ number accepted from a file or a name
	nicMaxRxFile     = 64      // bytes of a statistics/rx_packets file ("1234567890123\n")
)

// ifaceAddr is one network interface and its addresses.
type ifaceAddr struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []netip.Addr
}

// nicFS is how the rule reads the machine; a test points it at a fake tree
// and counts the calls. Names are full paths.
type nicFS interface {
	// readFile returns the file's contents, or an error when it is larger than
	// nicMaxFile.
	readFile(name string) ([]byte, error)
	// readDir returns at most limit entry names of a directory.
	readDir(name string, limit int) ([]string, error)
	// exists reports whether the name resolves (a symlink is followed).
	exists(name string) bool
}

// nicSource is where the rx-port rule reads the machine from.
type nicSource struct {
	sysClassNet string // /sys/class/net
	procDir     string // /proc
	ifaceAddrs  func() ([]ifaceAddr, error)
	fs          nicFS
}

func defaultNICSource() nicSource {
	return nicSource{
		sysClassNet: "/sys/class/net",
		procDir:     "/proc",
		ifaceAddrs:  osIfaceAddrs,
		fs:          osNICFS{},
	}
}

type osNICFS struct{}

var errNICFileTooBig = errors.New("file larger than the read cap")

func (osNICFS) readFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, nicMaxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > nicMaxFile {
		return nil, errNICFileTooBig
	}
	return b, nil
}

func (osNICFS) readDir(name string, limit int) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(limit)
	if errors.Is(err, io.EOF) {
		err = nil // an empty directory, or the end of a longer one
	}
	return names, err
}

func (osNICFS) exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// osIfaceAddrs lists the interfaces and their addresses with the net package
// (one netlink dump per interface; no cgo and no ip binary).
func osIfaceAddrs() ([]ifaceAddr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifaceAddr
	for _, ifc := range ifs {
		if len(out) >= 512 {
			break
		}
		a := ifaceAddr{
			Name:     ifc.Name,
			Up:       ifc.Flags&net.FlagUp != 0,
			Loopback: ifc.Flags&net.FlagLoopback != 0,
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, ad := range addrs {
			var ip net.IP
			switch v := ad.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if p, ok := netip.AddrFromSlice(ip); ok {
				a.Addrs = append(a.Addrs, p.Unmap())
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// satAdd is a+b, held at the largest uint64.
func satAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return ^uint64(0)
}

// atLeastPercent reports whether n is at least pct percent of total, exactly:
// n*100 >= total*pct over 128 bits, so a count near the top of uint64 neither
// wraps nor rounds.
func atLeastPercent(n, total, pct uint64) bool {
	hi1, lo1 := bits.Mul64(n, 100)
	hi2, lo2 := bits.Mul64(total, pct)
	return hi1 > hi2 || (hi1 == hi2 && lo1 >= lo2)
}

// validIfName reports whether name can be used as one path element under
// /sys/class/net: non-empty, no separator, not a dot name, no NUL, and short
// (the kernel's limit is 15 bytes).
func validIfName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, "/\x00")
}

// listenInterfaces resolves the listen address to interface names. A specific
// IP maps to the interface(s) that own it; a wildcard to every interface that
// is up, is not loopback and has a global unicast address; loopback and a host
// that is not an IP literal to none (a hostname would need a DNS lookup, which
// Listen does not do for this).
func listenInterfaces(addr string, src nicSource) (names []string, reason string) {
	host := addr
	if strings.HasPrefix(addr, "[") {
		end := strings.IndexByte(addr, ']')
		if end < 0 {
			return nil, "listen address is not host:port"
		}
		host = addr[1:end]
	} else if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		host = addr[:i]
	}
	wildcard := host == ""
	var ip netip.Addr
	zone := ""
	if !wildcard {
		var err error
		ip, err = netip.ParseAddr(host)
		if err != nil {
			return nil, "listen host is not an IP literal"
		}
		zone = ip.Zone()
		ip = ip.WithZone("").Unmap()
		if ip.IsLoopback() {
			return nil, "loopback listen address"
		}
		wildcard = ip.IsUnspecified()
	}
	all, err := src.ifaceAddrs()
	if err != nil {
		return nil, "cannot list interfaces"
	}
	for _, ifc := range all {
		if !validIfName(ifc.Name) || slices.Contains(names, ifc.Name) {
			continue
		}
		match := false
		if wildcard {
			match = ifc.Up && !ifc.Loopback
			if match {
				match = false
				for _, a := range ifc.Addrs {
					if a.WithZone("").Unmap().IsGlobalUnicast() {
						match = true
						break
					}
				}
			}
		} else if zone == "" || zone == ifc.Name {
			for _, a := range ifc.Addrs {
				if a.WithZone("").Unmap() == ip {
					match = true
					break
				}
			}
		}
		if match {
			names = append(names, ifc.Name)
			if len(names) >= nicMaxIfaces {
				break
			}
		}
	}
	if len(names) == 0 {
		if wildcard {
			return nil, "no up interface with a global address"
		}
		return nil, "no interface owns the listen address"
	}
	return names, ""
}

// lowerDevices follows the lower_<name> entries of each interface (bond, team,
// vlan, macvlan and bridge members all have them) to the interfaces that have a
// device link, the physical ones. The depth and the visited set bound it; a
// cycle ends at the second visit.
func lowerDevices(ifaces []string, src nicSource) []string {
	var devs []string
	visited := map[string]bool{}
	var walk func(name string, depth int)
	walk = func(name string, depth int) {
		if visited[name] || !validIfName(name) || len(visited) >= 4*nicMaxIfaces {
			return
		}
		visited[name] = true
		base := src.sysClassNet + "/" + name
		if src.fs.exists(base + "/device") {
			devs = append(devs, name)
			return
		}
		if depth >= nicMaxDepth {
			return
		}
		ents, err := src.fs.readDir(base, nicMaxDirEntries)
		if err != nil {
			return
		}
		// readdir order is the file system's: sorted, so that the order of the
		// ports (and with it which of them lists a shared vector first) does
		// not change between boots or file systems.
		slices.Sort(ents)
		for _, e := range ents {
			if lower, ok := strings.CutPrefix(e, "lower_"); ok {
				walk(lower, depth+1)
			}
		}
	}
	for _, n := range ifaces {
		walk(n, 0)
	}
	return devs
}

// deviceIRQs returns the IRQ numbers of one physical interface: the entries of
// device/msi_irqs (PCI MSI and MSI-X), else device/irq (INTx), else the
// /proc/interrupts rows whose action name is the interface's.
func deviceIRQs(dev string, src nicSource, rows []irqRow) ([]int, bool) {
	base := src.sysClassNet + "/" + dev + "/device"
	var irqs []int
	ents, err := src.fs.readDir(base+"/msi_irqs", nicMaxIRQsPerDev)
	switch {
	case err == nil:
		for _, e := range ents {
			if n, ok := parseIRQNumber(e); ok {
				irqs = append(irqs, n)
			}
		}
		slices.Sort(irqs)
		if len(irqs) > 0 {
			return irqs, true
		}
	case !errors.Is(err, fs.ErrNotExist):
		// Present but unreadable: do not guess from names. The detection
		// is given up (its share test needs every vector).
		return nil, false
	}
	b, err := src.fs.readFile(base + "/irq")
	switch {
	case err == nil:
		if n, ok := parseIRQNumber(strings.TrimSpace(string(b))); ok && n > 0 {
			return []int{n}, true
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, false
	}
	for _, r := range rows {
		if len(irqs) >= nicMaxIRQsPerDev {
			break
		}
		for _, name := range r.names {
			if actionMatchesIface(name, dev) {
				irqs = append(irqs, r.irq)
				break
			}
		}
	}
	slices.Sort(irqs)
	return irqs, true
}

// actionMatchesIface reports whether an /proc/interrupts action name belongs to
// the interface: the name itself, or the name followed by one of "-", ":", "@",
// "_" and a suffix ("eth0-TxRx-0", "eth0:tx-0"). Without the separator "eth1"
// would claim the vectors of "eth10".
func actionMatchesIface(action, iface string) bool {
	rest, ok := strings.CutPrefix(action, iface)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	switch rest[0] {
	case '-', ':', '@', '_':
		return true
	}
	return false
}

// parseIRQNumber parses a decimal IRQ number (a directory name or a file's
// content) before it is put into a path.
func parseIRQNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > nicMaxIRQ {
		return 0, false
	}
	return n, true
}

// isCPUList reports whether s is a well-formed kernel CPU list: comma-separated
// numbers and ascending ranges ("0-1,6-11").
func isCPUList(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 1<<16 {
		return false
	}
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, ok := parseCPUNumber(lo)
		if !ok {
			return false
		}
		if isRange {
			if b, ok := parseCPUNumber(hi); !ok || b < a {
				return false
			}
		}
	}
	return true
}

// parseSingleCPU parses a kernel CPU list that names exactly one CPU ("4",
// "4-4"). A list of several CPUs, an empty one and anything malformed return
// false.
func parseSingleCPU(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 24 {
		return 0, false
	}
	lo, hi, isRange := strings.Cut(s, "-")
	a, ok := parseCPUNumber(lo)
	if !ok {
		return 0, false
	}
	if isRange {
		b, ok := parseCPUNumber(hi)
		if !ok || b != a {
			return 0, false
		}
	}
	return a, true
}

func parseCPUNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n >= nicMaxCPUs {
		return 0, false
	}
	return n, true
}

// irqRow is one numbered line of /proc/interrupts.
type irqRow struct {
	irq    int
	cpus   []int    // CPU of each counted column, from the header
	counts []uint64 // interrupts per column
	total  uint64
	label  string   // the last word of the description: the action name
	names  []string // every word of the description, commas split off
}

// parseInterrupts parses /proc/interrupts. The header names the CPU of each
// column ("CPU0 CPU1 CPU4": offline CPUs have no column, so the columns are
// mapped through the header and never by position). A numbered row yields its
// counts and its description; a row with fewer numeric columns than the header
// (or a count that does not parse) is dropped rather than guessed at, and the
// rows without a number (NMI, LOC, ERR) are skipped. A missing or unexpected
// header yields nothing.
func parseInterrupts(b []byte) []irqRow {
	s := string(b)
	var cpus []int
	headerSeen := false
	var rows []irqRow
	for len(s) > 0 {
		var line string
		line, s, _ = strings.Cut(s, "\n")
		if !headerSeen {
			if strings.TrimSpace(line) == "" {
				continue
			}
			headerSeen = true
			for f := range strings.FieldsSeq(line) {
				n, ok := strings.CutPrefix(f, "CPU")
				if !ok {
					return nil
				}
				c, ok := parseCPUNumber(n)
				if !ok || len(cpus) >= nicMaxCPUs {
					return nil
				}
				cpus = append(cpus, c)
			}
			if len(cpus) == 0 {
				return nil
			}
			continue
		}
		label, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		irq, ok := parseIRQNumber(strings.TrimSpace(label))
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < len(cpus) {
			continue
		}
		row := irqRow{irq: irq, cpus: cpus, counts: make([]uint64, len(cpus))}
		bad := false
		for i := range cpus {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				bad = true
				break
			}
			row.counts[i] = v
			if row.total+v < row.total {
				row.total = ^uint64(0)
			} else {
				row.total += v
			}
		}
		if bad {
			continue
		}
		for _, w := range fields[len(cpus):] {
			for part := range strings.SplitSeq(w, ",") {
				if part != "" {
					row.names = append(row.names, part)
				}
			}
		}
		if n := len(row.names); n > 0 {
			row.label = row.names[n-1]
		}
		rows = append(rows, row)
	}
	return rows
}

// effectiveCPU names the CPU that services irq, or -1. In order: the single CPU
// of /proc/irq/N/effective_affinity_list, then the single CPU of
// smp_affinity_list. A source is passed over only when its file is absent,
// empty or names several CPUs (a mask says nothing about which one services the
// vector). A file that cannot be read, or is not a CPU list, ends the search
// with -1: a read error is never worked around with older data. There is no
// fallback to the CPU that took most of the interrupts since boot: that is
// history, and on msr1 it names CPU 4 for IRQ 167 when the vector sits on CPU 1
// (2.6e9 interrupts counted on CPU 4 before it was moved, 6e7 on CPU 1).
func effectiveCPU(irq int, src nicSource) int {
	dir := src.procDir + "/irq/" + strconv.Itoa(irq)
	for _, f := range []string{"effective_affinity_list", "smp_affinity_list"} {
		b, err := src.fs.readFile(dir + "/" + f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return -1
		}
		if strings.TrimSpace(string(b)) == "" {
			// An empty list: a vector not activated, or an irqchip that does
			// not update the effective mask. It names no CPU, like an absent
			// file, and the next source is read.
			continue
		}
		if c, ok := parseSingleCPU(string(b)); ok {
			return c
		}
		if !isCPUList(string(b)) {
			return -1
		}
	}
	return -1
}

// parseRxPackets parses the content of /sys/class/net/<if>/statistics/rx_packets:
// one decimal number, a newline. Anything else (empty, signed, hex, trailing
// text, longer than a uint64) is rejected rather than guessed at.
func parseRxPackets(b []byte) (uint64, bool) {
	s := strings.TrimSpace(string(b))
	if s == "" || len(s) > 20 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readRxPackets reads the packets a physical interface received since boot.
// The counter is 64 bits wide and does not wrap in practice.
func readRxPackets(dev string, src nicSource) (uint64, bool) {
	b, err := src.fs.readFile(src.sysClassNet + "/" + dev + "/statistics/rx_packets")
	if err != nil || len(b) > nicMaxRxFile {
		return 0, false
	}
	return parseRxPackets(b)
}
