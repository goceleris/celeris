//go:build linux

package platform

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// rxHost is a fake /sys/class/net, /proc/irq and /proc/interrupts tree under
// t.TempDir(), the interface table the net package would report, and a count
// of the reads a plan did. The interrupt report's tests (nicirq_linux_test.go,
// celeris#1062) build on it through nicHost.
type rxHost struct {
	t      testing.TB
	root   string
	ifaces []ifaceAddr
	// ifaceErr, when set, is what ifaceAddrs returns.
	ifaceErr error
	// ifaceCalls counts the interface-table calls (a netlink dump, not a file
	// read); fs counts the file reads.
	ifaceCalls int
	fs         *rxCountFS
	rows       []string
	cpus       []int
}

// rxCountFS is the real file system reads over the fake tree, counted, with
// optional rules that fail or panic on a path.
type rxCountFS struct {
	inner osNICFS
	calls int
	deny  func(name string) bool
	boom  func(name string) bool
	// reverse returns directory entries in descending name order: the order
	// of a real readdir is whatever the file system likes (hash order on
	// ext4, creation order on tmpfs), and the code must not depend on it.
	reverse bool
}

func (c *rxCountFS) check(name string) error {
	c.calls++
	if c.boom != nil && c.boom(name) {
		panic("rxCountFS: boom " + name)
	}
	if c.deny != nil && c.deny(name) {
		return os.ErrPermission
	}
	return nil
}

func (c *rxCountFS) readFile(name string) ([]byte, error) {
	if err := c.check(name); err != nil {
		return nil, err
	}
	return c.inner.readFile(name)
}

func (c *rxCountFS) readDir(name string, limit int) ([]string, error) {
	if err := c.check(name); err != nil {
		return nil, err
	}
	names, err := c.inner.readDir(name, limit)
	if c.reverse {
		slices.SortFunc(names, func(a, b string) int { return strings.Compare(b, a) })
	}
	return names, err
}

func (c *rxCountFS) exists(name string) bool {
	if err := c.check(name); err != nil {
		return false
	}
	return c.inner.exists(name)
}

func newRxHost(t testing.TB) *rxHost {
	t.Helper()
	h := &rxHost{t: t, root: t.TempDir(), fs: &rxCountFS{}, cpus: rng(0, 11)}
	h.mkdir("sys/class/net")
	h.mkdir("proc/irq")
	return h
}

func (h *rxHost) path(rel string) string { return filepath.Join(h.root, rel) }

func (h *rxHost) mkdir(rel string) {
	h.t.Helper()
	if err := os.MkdirAll(h.path(rel), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *rxHost) write(rel, content string) {
	h.t.Helper()
	h.mkdir(filepath.Dir(rel))
	if err := os.WriteFile(h.path(rel), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// phys adds an interface with a device link (a physical NIC) and its
// msi_irqs entries.
func (h *rxHost) phys(name string, irqs ...int) {
	h.t.Helper()
	h.mkdir("sys/class/net/" + name + "/device")
	if len(irqs) > 0 {
		h.mkdir("sys/class/net/" + name + "/device/msi_irqs")
	}
	for _, irq := range irqs {
		h.write("sys/class/net/"+name+"/device/msi_irqs/"+strconv.Itoa(irq), "msix\n")
	}
}

// virt adds an interface with no device link (bond, vlan, bridge).
func (h *rxHost) virt(name string) {
	h.t.Helper()
	h.mkdir("sys/class/net/" + name)
}

// lower adds the lower_<lower> link of upper, the way the kernel does.
func (h *rxHost) lower(upper, lower string) {
	h.t.Helper()
	h.mkdir("sys/class/net/" + upper)
	if err := os.Symlink("../"+lower, h.path("sys/class/net/"+upper+"/lower_"+lower)); err != nil {
		h.t.Fatal(err)
	}
}

// addr declares an interface the net package lists.
func (h *rxHost) addr(name string, up bool, addrs ...string) {
	h.t.Helper()
	a := ifaceAddr{Name: name, Up: up, Loopback: name == "lo"}
	for _, s := range addrs {
		a.Addrs = append(a.Addrs, netip.MustParseAddr(s))
	}
	h.ifaces = append(h.ifaces, a)
}

// irq writes /proc/irq/N/{effective_affinity_list,smp_affinity_list}; an empty
// string leaves that file out.
func (h *rxHost) irq(n int, effective, smp string) {
	h.t.Helper()
	if effective != "" {
		h.write(fmt.Sprintf("proc/irq/%d/effective_affinity_list", n), effective+"\n")
	}
	if smp != "" {
		h.write(fmt.Sprintf("proc/irq/%d/smp_affinity_list", n), smp+"\n")
	}
}

// rx writes statistics/rx_packets of an interface.
func (h *rxHost) rx(dev string, packets uint64) {
	h.t.Helper()
	h.write("sys/class/net/"+dev+"/statistics/rx_packets", strconv.FormatUint(packets, 10)+"\n")
}

// intr adds a numbered /proc/interrupts row: per-CPU counts by CPU (the others
// are 0) and the action name.
func (h *rxHost) intr(irq int, action string, counts map[int]uint64) {
	var b strings.Builder
	fmt.Fprintf(&b, "%3d:", irq)
	for _, c := range h.cpus {
		fmt.Fprintf(&b, " %10d", counts[c])
	}
	fmt.Fprintf(&b, "  ITS-MSI %d Edge      %s", 524288+irq, action)
	h.rows = append(h.rows, b.String())
}

// flushInterrupts writes /proc/interrupts: the header, the rows, and the
// unnumbered rows a real file has.
func (h *rxHost) flushInterrupts() {
	h.t.Helper()
	var b strings.Builder
	b.WriteString("          ")
	for _, c := range h.cpus {
		fmt.Fprintf(&b, " %10s", "CPU"+strconv.Itoa(c))
	}
	b.WriteString("\n")
	for _, r := range h.rows {
		b.WriteString(r + "\n")
	}
	b.WriteString("IPI0:" + strings.Repeat("          0", len(h.cpus)) + "  Rescheduling interrupts\n")
	b.WriteString("Err:          0\n")
	h.write("proc/interrupts", b.String())
}

func (h *rxHost) nic() nicSource {
	h.t.Helper()
	return nicSource{
		sysClassNet: h.path("sys/class/net"),
		procDir:     h.path("proc"),
		ifaceAddrs: func() ([]ifaceAddr, error) {
			h.ifaceCalls++
			return h.ifaces, h.ifaceErr
		},
		fs: h.fs,
	}
}

// reads is the number of file reads and interface-table calls so far.
func (h *rxHost) reads() int { return h.fs.calls + h.ifaceCalls }

// planSrc joins the CPU side (a fakeHost) with this NIC tree and a listen
// address.
func (h *rxHost) planSrc(cpu *fakeHost, addr string) planSource {
	s := cpu.src()
	n := h.nic()
	s.nic, s.addr = &n, addr
	return s
}

// The listener of the cluster cells: the default Workers (GOMAXPROCS, 12).
const (
	clusterAddr = "0.0.0.0:8080"
	bondAddr    = "192.168.50.199:8080"
)

// msr1RxReal builds the host of a testdata snapshot (testdata/msr1_rxfree_*.txt):
// bond0 (192.168.50.199) over enp1s0 and enp49s0, the other interfaces the
// host had up (loopback, tailscale0, docker0, which have no device), the 64
// NIC vectors found by their PCI address, /proc/interrupts with a
// CPU0..CPU11 header, the effective and smp affinity of every vector, and
// statistics/rx_packets of the two ports from the snapshot's /proc/net/dev.
func msr1RxReal(t testing.TB, file string) *rxHost {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	h := newRxHost(t)
	var irqLines, affLines, netLines []string
	section := ""
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(l, "# "), strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "== irq effective_affinity_list"):
			section = "aff"
		case strings.HasPrefix(l, "== irq"):
			section = "irq"
		case strings.HasPrefix(l, "== /proc/net/dev"):
			section = "net"
		case strings.HasPrefix(l, "== "):
			section = ""
		case section == "irq":
			irqLines = append(irqLines, l)
		case section == "aff":
			affLines = append(affLines, l)
		case section == "net":
			netLines = append(netLines, l)
		}
	}
	// Vectors by device: the PCI address in the row names the port (bus 0x31 is
	// enp49s0, bus 0x01 is enp1s0).
	pci := regexp.MustCompile(`ITS-PCI-MSIX-0000:([0-9a-f]{2}):00\.0`)
	byDev := map[string][]int{}
	for _, l := range irqLines {
		m := pci.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		bus, _ := strconv.ParseUint(m[1], 16, 8)
		irq, _ := strconv.Atoi(strings.TrimSuffix(strings.Fields(l)[0], ":"))
		dev := fmt.Sprintf("enp%ds0", bus)
		byDev[dev] = append(byDev[dev], irq)
	}
	if len(byDev["enp1s0"]) != 32 || len(byDev["enp49s0"]) != 32 {
		t.Fatalf("snapshot %s: %d and %d vectors, want 32 and 32", file, len(byDev["enp1s0"]), len(byDev["enp49s0"]))
	}
	h.phys("enp1s0", byDev["enp1s0"]...)
	h.phys("enp49s0", byDev["enp49s0"]...)
	h.virt("bond0")
	h.lower("bond0", "enp1s0")
	h.lower("bond0", "enp49s0")
	h.virt("tailscale0")
	h.virt("docker0")
	h.addr("lo", true, "127.0.0.1", "::1")
	h.addr("enp49s0", true)
	h.addr("enp1s0", true)
	h.addr("bond0", true, "192.168.50.199", "fe80::a6e4:7cff:fe79:9662")
	h.addr("tailscale0", true, "100.64.0.7")
	h.addr("docker0", true, "172.17.0.1")
	for _, l := range affLines {
		f := strings.Fields(l)
		if len(f) != 3 {
			t.Fatalf("snapshot %s: affinity line %q", file, l)
		}
		irq, _ := strconv.Atoi(f[0])
		h.irq(irq, f[1], f[2])
	}
	// /proc/net/dev: "enp49s0: <rx bytes> <rx packets> ..." and the same for enp1s0.
	got := 0
	for _, l := range netLines {
		name, rest, ok := strings.Cut(l, ":")
		name = strings.TrimSpace(name)
		if !ok || (name != "enp1s0" && name != "enp49s0") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			t.Fatalf("snapshot %s: net/dev line %q", file, l)
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			t.Fatalf("snapshot %s: net/dev line %q: %v", file, l, err)
		}
		h.rx(name, n)
		got++
	}
	if got != 2 {
		t.Fatalf("snapshot %s: %d of the 2 ports in /proc/net/dev", file, got)
	}
	var b strings.Builder
	b.WriteString("          ")
	for c := 0; c < 12; c++ {
		fmt.Fprintf(&b, " %10s", "CPU"+strconv.Itoa(c))
	}
	b.WriteString("\n" + strings.Join(irqLines, "\n") + "\n")
	h.write("proc/interrupts", b.String())
	return h
}
