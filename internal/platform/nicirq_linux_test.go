//go:build linux

package platform

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// nicHost is the fake tree of nicrx_fake_linux_test.go (rxHost, celeris#1085)
// with one addition for the report's tests: a fixture that writes no
// /proc/interrupts gets a busy one (fillCounts).
type nicHost struct {
	*rxHost
	// noInterrupts leaves /proc/interrupts out, however the fixture is built.
	noInterrupts bool
}

func newNICHost(t testing.TB) *nicHost { return &nicHost{rxHost: newRxHost(t)} }

// fillCounts gives a fixture that writes no /proc/interrupts a busy one: every
// vector under /sys/class/net/*/device gets 1000 interrupts on each CPU, so
// none is idle or dominated by one CPU. The topology tests (which interface,
// which device, which affinity file) say nothing about counts, and since the
// traffic test (celeris#1036) a vector with no counts is no vector.
func (h *nicHost) fillCounts() {
	h.t.Helper()
	if h.noInterrupts || len(h.rows) > 0 {
		return
	}
	if _, err := os.Stat(h.path("proc/interrupts")); err == nil {
		return
	}
	even := map[int]uint64{}
	for _, c := range h.cpus {
		even[c] = 1000
	}
	devs, _ := filepath.Glob(h.path("sys/class/net/*/device"))
	for _, d := range devs {
		var irqs []int
		ents, _ := os.ReadDir(d + "/msi_irqs")
		for _, e := range ents {
			if n, err := strconv.Atoi(e.Name()); err == nil {
				irqs = append(irqs, n)
			}
		}
		if b, err := os.ReadFile(d + "/irq"); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				irqs = append(irqs, n)
			}
		}
		slices.Sort(irqs)
		for i, n := range irqs {
			h.intr(n, "fill"+strconv.Itoa(i), even) // a name no interface matches
		}
	}
	h.flushInterrupts()
}

func (h *nicHost) src() nicSource {
	h.t.Helper()
	h.fillCounts()
	return h.nic()
}

func (h *nicHost) detect(addr string) NICIRQs { return detectNICIRQs(addr, h.src()) }

// msr1NIC is the shape of the NICs of the host the stalls were seen on: bond0
// (192.168.50.199) over enp1s0 and enp49s0, two vectors each. enp1s0-8 (IRQ
// 167) is serviced by CPU 4, a little CPU.
func msr1NIC(t testing.TB) *nicHost {
	t.Helper()
	h := newNICHost(t)
	h.phys("enp1s0", 159, 167)
	h.phys("enp49s0", 127, 135)
	h.virt("bond0")
	h.lower("bond0", "enp1s0")
	h.lower("bond0", "enp49s0")
	h.addr("lo", true, "127.0.0.1", "::1")
	h.addr("enp1s0", true)
	h.addr("enp49s0", true)
	h.addr("bond0", true, "192.168.50.199", "fe80::1")
	h.irq(159, "0", "0-11")
	h.irq(167, "4", "0-11")
	h.irq(127, "6", "0-11")
	h.irq(135, "8", "0-11")
	// 117000 interrupts in all: every vector has the floor (1000) and a share
	// of at least 5% (6.8%, 7.7%, 59.8%, 25.6%).
	h.intr(159, "enp1s0-0", map[int]uint64{0: 8000})
	h.intr(167, "enp1s0-8", map[int]uint64{4: 9000})
	h.intr(127, "enp49s0-0", map[int]uint64{6: 70000})
	h.intr(135, "enp49s0-8", map[int]uint64{8: 30000})
	h.flushInterrupts()
	return h
}

func vecCPUs(v NICIRQs) []int {
	var out []int
	for _, x := range v.Vectors {
		if !slices.Contains(out, x.CPU) {
			out = append(out, x.CPU)
		}
	}
	slices.Sort(out)
	return out
}

func vecByIRQ(t *testing.T, v NICIRQs, irq int) NICVector {
	t.Helper()
	for _, x := range v.Vectors {
		if x.IRQ == irq {
			return x
		}
	}
	t.Fatalf("no vector for IRQ %d in %+v", irq, v.Vectors)
	return NICVector{}
}

func TestDetectMSR1Bond(t *testing.T) {
	h := msr1NIC(t)
	got := h.detect("192.168.50.199:8080")
	eq(t, "serviced CPUs", vecCPUs(got), []int{0, 4, 6, 8})
	eq(t, "ifaces", got.Ifaces, []string{"bond0"})
	eq(t, "devs", got.Devs, []string{"enp1s0", "enp49s0"})
	if got.Reason != "" || got.Unknown != 0 {
		t.Errorf("reason %q unknown %d, want none", got.Reason, got.Unknown)
	}
	v := vecByIRQ(t, got, 167)
	eq(t, "IRQ 167", v, NICVector{IRQ: 167, CPU: 4, Name: "enp1s0-8", Dev: "enp1s0", Count: 9000})
	if x := vecByIRQ(t, got, 135); x.CPU != 8 || x.Name != "enp49s0-8" || x.Count != 30000 {
		t.Errorf("IRQ 135 = %+v", x)
	}
}

func TestDetectListenAddresses(t *testing.T) {
	h := msr1NIC(t)
	// A second, single NIC with its own address, and the rest a wildcard
	// listener must leave out.
	h.phys("wlan0", 90)
	h.irq(90, "10", "")
	h.intr(90, "wlan0", map[int]uint64{10: 12000})
	h.flushInterrupts()
	h.addr("enp7s0", true, "10.0.0.5")
	h.phys("enp7s0", 70)
	h.irq(70, "5", "")
	h.intr(70, "enp7s0-0", map[int]uint64{5: 12000})
	h.flushInterrupts()
	h.addr("wlan0", true, "192.168.1.9")
	h.addr("eth9", true)                 // up, no address
	h.addr("docker0", true, "fe80::42")  // link-local only
	h.addr("down0", false, "172.16.0.2") // down
	h.phys("down0", 80)
	h.irq(80, "11", "")

	cases := []struct {
		name string
		addr string
		want []int
		why  string
	}{
		{"specific IP of the bond", "192.168.50.199:8080", []int{0, 4, 6, 8}, ""},
		{"specific IP of the single NIC", "10.0.0.5:80", []int{5}, ""},
		{"loopback v4", "127.0.0.1:8080", nil, "loopback"},
		{"loopback v6", "[::1]:8080", nil, "loopback"},
		{"IP no interface owns", "10.9.9.9:80", nil, "no interface owns"},
		{"not an IP literal", "localhost:8080", nil, "not an IP literal"},
		{"unparsable", "[::1:80", nil, "not host:port"},
		{"wildcard :port", ":8080", []int{0, 4, 5, 6, 8, 10}, ""},
		{"wildcard 0.0.0.0", "0.0.0.0:0", []int{0, 4, 5, 6, 8, 10}, ""},
		{"wildcard [::]", "[::]:8080", []int{0, 4, 5, 6, 8, 10}, ""},
		{"v4-mapped IP of the bond", "[::ffff:192.168.50.199]:80", []int{0, 4, 6, 8}, ""},
		{"zoned link-local of the bond", "[fe80::1%bond0]:80", []int{0, 4, 6, 8}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.detect(tc.addr)
			eq(t, "serviced CPUs", vecCPUs(got), tc.want)
			if tc.why == "" && got.Reason != "" {
				t.Errorf("reason %q, want none", got.Reason)
			}
			if tc.why != "" && !strings.Contains(got.Reason, tc.why) {
				t.Errorf("reason %q, want it to say %q", got.Reason, tc.why)
			}
		})
	}
}

func TestDetectZoneRestrictsTheInterface(t *testing.T) {
	// The same link-local address on two interfaces: a zone names one.
	h := newNICHost(t)
	h.phys("eth0", 1)
	h.phys("eth1", 2)
	h.irq(1, "3", "")
	h.irq(2, "5", "")
	h.addr("eth0", true, "fe80::1")
	h.addr("eth1", true, "fe80::1")
	eq(t, "zone eth0", vecCPUs(h.detect("[fe80::1%eth0]:80")), []int{3})
	eq(t, "zone eth1", vecCPUs(h.detect("[fe80::1%eth1]:80")), []int{5})
	eq(t, "no zone", vecCPUs(h.detect("[fe80::1]:80")), []int{3, 5})
	if got := h.detect("[fe80::1%eth9]:80"); len(got.Vectors) != 0 {
		t.Errorf("zone of another interface: %+v", got)
	}
}

func TestDetectIfaceTableError(t *testing.T) {
	h := msr1NIC(t)
	h.ifaceErr = os.ErrPermission
	got := h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || got.Reason == "" {
		t.Errorf("got %+v, want an empty result with a reason", got)
	}
}

func TestDetectStackedInterfaces(t *testing.T) {
	// vlan100 -> bond0 -> two slaves; a bridge over the vlan; a macvlan.
	h := newNICHost(t)
	h.phys("enp1s0", 10)
	h.phys("enp2s0", 20)
	h.virt("bond0")
	h.lower("bond0", "enp1s0")
	h.lower("bond0", "enp2s0")
	h.virt("vlan100")
	h.lower("vlan100", "bond0")
	h.virt("br0")
	h.lower("br0", "vlan100")
	h.virt("macvlan0")
	h.lower("macvlan0", "br0")
	h.irq(10, "3", "")
	h.irq(20, "5", "")
	h.addr("br0", true, "10.1.1.1")
	h.addr("macvlan0", true, "10.1.1.2")
	h.addr("vlan100", true, "10.1.2.1")
	h.addr("bond0", true, "10.1.3.1")

	for _, a := range []string{"10.1.1.1:1", "10.1.1.2:1", "10.1.2.1:1", "10.1.3.1:1"} {
		got := h.detect(a)
		eq(t, a+" devs", got.Devs, []string{"enp1s0", "enp2s0"})
		eq(t, a+" CPUs", vecCPUs(got), []int{3, 5})
	}
}

func TestDetectLowerCycleAndDepth(t *testing.T) {
	h := newNICHost(t)
	h.virt("a")
	h.virt("b")
	h.lower("a", "b")
	h.lower("b", "a")
	h.addr("a", true, "10.2.0.1")
	got := h.detect("10.2.0.1:1")
	if len(got.Vectors) != 0 || got.Reason == "" {
		t.Errorf("cycle: got %+v, want an empty result with a reason", got)
	}

	// A chain deeper than the cap stops at the cap, and the device below it
	// is not reached.
	h = newNICHost(t)
	h.phys("leaf", 5)
	h.irq(5, "2", "")
	prev := "leaf"
	for i := 0; i < nicMaxDepth+2; i++ {
		name := "v" + strconv.Itoa(i)
		h.virt(name)
		h.lower(name, prev)
		prev = name
	}
	h.addr(prev, true, "10.3.0.1")
	if got := h.detect("10.3.0.1:1"); len(got.Vectors) != 0 {
		t.Errorf("chain past depth %d reached the device: %+v", nicMaxDepth, got)
	}
	// The boundary: the device at depth nicMaxDepth is found, one more link
	// down is not (an interface at that depth is not followed any further).
	for _, tc := range []struct {
		virts int
		found bool
	}{{nicMaxDepth, true}, {nicMaxDepth + 1, false}} {
		b := newNICHost(t)
		b.phys("leaf", 5)
		b.irq(5, "2", "")
		prev := "leaf"
		for i := range tc.virts {
			name := "w" + strconv.Itoa(i)
			b.virt(name)
			b.lower(name, prev)
			prev = name
		}
		b.addr(prev, true, "10.4.0.1")
		got := b.detect("10.4.0.1:1")
		if (len(got.Vectors) == 1) != tc.found {
			t.Errorf("%d virtual links above the device: found=%v, want %v (%+v)", tc.virts, len(got.Vectors) == 1, tc.found, got)
		}
	}
	// Within the cap it is found.
	h.addr("v1", true, "10.3.0.2")
	if got := h.detect("10.3.0.2:1"); !slices.Equal(vecCPUs(got), []int{2}) {
		t.Errorf("chain within the cap: got %+v", got)
	}
}

func TestDetectContainer(t *testing.T) {
	// A pod's netns: only a veth, no device link, no /proc/irq entries.
	h := newNICHost(t)
	h.virt("eth0")
	h.addr("lo", true, "127.0.0.1")
	h.addr("eth0", true, "172.17.0.2")
	for _, a := range []string{"172.17.0.2:80", ":80"} {
		got := h.detect(a)
		if len(got.Vectors) != 0 || got.Reason == "" {
			t.Errorf("%s: got %+v, want an empty result with a reason", a, got)
		}
	}
	// No /sys/class/net at all.
	h2 := newNICHost(t)
	h2.addr("eth0", true, "172.17.0.2")
	if got := h2.detect("172.17.0.2:80"); len(got.Vectors) != 0 || got.Reason == "" {
		t.Errorf("no sysfs entry: got %+v", got)
	}
}

func TestDetectUnreadableFiles(t *testing.T) {
	deny := func(prefix string) func(string) bool {
		return func(name string) bool { return strings.Contains(name, prefix) }
	}
	// Every /proc/irq file refused (EACCES): the CPU of no vector is known, and
	// /proc/interrupts is not used to work around it.
	h := msr1NIC(t)
	h.fs.deny = deny("/proc/irq/")
	got := h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || got.Unknown != 4 || got.Reason == "" {
		t.Errorf("with /proc/irq refused: got %+v, want no vector, 4 unknown and a reason", got)
	}
	base := msr1Plan(t, 12, false, rng(0, 11))
	h.fs.deny = deny("/proc/irq/")
	pl := annotatePlanNIC(base, "192.168.50.199:8080", h.src())
	eq(t, "loops with /proc/irq refused", pl.CPUs, base.CPUs)
	eq(t, "Note with /proc/irq refused", pl.Note, base.Note)
	if len(pl.IRQCPUs) != 0 || len(pl.IRQUncovered) != 0 {
		t.Errorf("plan with /proc/irq refused carries IRQ state: %+v", pl)
	}

	// msi_irqs refused: the device is not guessed at from the names in
	// /proc/interrupts (enp1s0-8 would match), and since the share test needs
	// every vector of every port, one port that cannot be counted ends the
	// detection: not even the other port's vectors are reported.
	h = msr1NIC(t)
	h.fs.deny = deny("/msi_irqs")
	got = h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || !strings.Contains(got.Reason, "cannot read the interrupt vectors of enp1s0") {
		t.Errorf("with msi_irqs refused: got %+v, want none and the reason", got)
	}
	h = msr1NIC(t)
	h.fs.deny = func(name string) bool {
		return strings.Contains(name, "/enp49s0/") && strings.Contains(name, "/msi_irqs")
	}
	got = h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || !strings.Contains(got.Reason, "cannot read the interrupt vectors of enp49s0") {
		t.Errorf("with the second port's msi_irqs refused: got %+v, want none (its traffic is not counted) and the reason", got)
	}

	// Everything refused.
	h = msr1NIC(t)
	h.fs.deny = func(string) bool { return true }
	got = h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || got.Reason == "" {
		t.Errorf("everything refused: got %+v", got)
	}
	// /proc/interrupts refused only: without the counts no vector can be told
	// from an idle one, so none is reported (before the traffic test they
	// resolved with no names and a count of 0, and earned a loop).
	h = msr1NIC(t)
	h.fs.deny = deny("/proc/interrupts")
	got = h.detect("192.168.50.199:8080")
	if len(got.Vectors) != 0 || got.Reason == "" || got.Idle != 0 || got.Negligible != 0 {
		t.Errorf("without /proc/interrupts: got %+v, want no vector, no skip count and a reason", got)
	}
	pl = annotatePlanNIC(base, "192.168.50.199:8080", h.src())
	eq(t, "loops without /proc/interrupts", pl.CPUs, base.CPUs)
	eq(t, "Note without /proc/interrupts", pl.Note, base.Note)
}

func TestDetectAffinitySources(t *testing.T) {
	cases := []struct {
		name      string
		effective string
		smp       string
		counts    map[int]uint64
		wantCPU   int // -1: skipped
	}{
		{"effective single", "4", "0-11", nil, 4},
		{"effective range of one", "4-4", "", nil, 4},
		{"effective wins over a different single smp", "4", "7", nil, 4},
		{"effective missing, smp single", "", "4", nil, 4},
		{"effective missing, smp mask, no counts", "", "0-11", nil, -1},
		{"effective multi, smp mask", "0-11", "0-11", nil, -1},
		{"effective list of two", "4,6", "", nil, -1},
		{"effective range of two", "4-5", "", nil, -1},
		{"effective garbage, counts would say 4", "x", "", map[int]uint64{4: 9500}, -1},
		{"effective garbage, smp single", "x", "4", nil, -1},
		{"both files missing, one CPU took the interrupts: history, not a CPU", "", "", map[int]uint64{4: 9500, 5: 500}, -1},
		{"both files missing, no counts", "", "", nil, -1},
		{"effective too long", strings.Repeat("1", 30), "", nil, -1},
		{"effective huge", "99999999999", "", nil, -1},
		{"mask but one CPU took 95 percent: history, not a CPU", "0-11", "0-11", map[int]uint64{4: 9500, 5: 500}, -1},
		{"mask, counts spread", "0-11", "0-11", map[int]uint64{4: 500, 5: 500}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newNICHost(t)
			h.phys("eth0", 50)
			h.virt("bond0")
			h.lower("bond0", "eth0")
			h.addr("bond0", true, "10.0.0.1")
			h.irq(50, tc.effective, tc.smp)
			if tc.counts != nil {
				h.intr(50, "eth0-0", tc.counts)
				h.flushInterrupts()
			}
			got := h.detect("10.0.0.1:1")
			if tc.wantCPU < 0 {
				if len(got.Vectors) != 0 || got.Unknown != 1 {
					t.Errorf("got %+v unknown=%d, want the vector skipped as unknown", got.Vectors, got.Unknown)
				}
				return
			}
			if len(got.Vectors) != 1 || got.Vectors[0].CPU != tc.wantCPU {
				t.Errorf("got %+v, want CPU %d", got.Vectors, tc.wantCPU)
			}
		})
	}
}

func TestDetectPlatformDeviceNameFallback(t *testing.T) {
	// No msi_irqs and no irq file: the vectors are found by the action name.
	// eth10-0 must not be taken for eth1's, and eth0:tx-0 / eth0:rx-0 are
	// eth0's.
	// Every vector has taken an interrupt: a vector that never fired earns no
	// loop, whatever finds it.
	one := map[int]uint64{0: nicMinInterrupts}
	build := func() *nicHost {
		h := newNICHost(t)
		h.phys("eth0")
		h.phys("eth1")
		h.phys("eth10")
		h.addr("eth0", true, "10.0.0.1")
		h.addr("eth1", true, "10.0.1.1")
		h.addr("eth10", true, "10.0.10.1")
		h.intr(30, "eth0:tx-0", one)
		h.intr(31, "eth0:rx-0", one)
		h.intr(32, "eth10-0", one)
		h.intr(33, "eth1-0", one)
		h.intr(34, "eth1x", one)
		h.intr(35, "eth1", one)
		h.intr(36, "ahci, eth1@pci:0000:01:00.0", one)
		h.intr(37, "xeth1-0", one)
		for i, cpu := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
			h.irq(30+i, strconv.Itoa(cpu), "")
		}
		h.flushInterrupts()
		return h
	}
	h := build()
	got := h.detect("10.0.0.1:1")
	eq(t, "eth0 IRQs", irqsOf(got), []int{30, 31})
	got = h.detect("10.0.1.1:1")
	eq(t, "eth1 IRQs (not eth10, eth1x, xeth1)", irqsOf(got), []int{33, 35, 36})
	got = h.detect("10.0.10.1:1")
	eq(t, "eth10 IRQs", irqsOf(got), []int{32})
}

func irqsOf(v NICIRQs) []int {
	var out []int
	for _, x := range v.Vectors {
		out = append(out, x.IRQ)
	}
	slices.Sort(out)
	return out
}

func TestDetectINTxFallback(t *testing.T) {
	h := newNICHost(t)
	h.phys("eth0")
	h.write("sys/class/net/eth0/device/irq", "17\n")
	h.irq(17, "2", "")
	h.addr("eth0", true, "10.0.0.1")
	got := h.detect("10.0.0.1:1")
	eq(t, "INTx IRQ", irqsOf(got), []int{17})
	eq(t, "INTx CPU", vecCPUs(got), []int{2})
	// A zero or garbage irq file names nothing (IRQ 0 is the timer, and a
	// directory for it exists).
	h.irq(0, "3", "")
	h.intr(0, "timer", map[int]uint64{3: 900000}) // busy, so that only the number keeps it out
	h.flushInterrupts()
	h.write("sys/class/net/eth0/device/irq", "0\n")
	if got := h.detect("10.0.0.1:1"); len(got.Vectors) != 0 {
		t.Errorf("irq 0: got %+v", got.Vectors)
	}
}

func TestDetectSharedINTxCountsOnce(t *testing.T) {
	// Two ports of a bond on the same legacy line: one vector, not two (its
	// load would be counted twice).
	h := newNICHost(t)
	h.phys("a")
	h.phys("b")
	h.write("sys/class/net/a/device/irq", "17\n")
	h.write("sys/class/net/b/device/irq", "17\n")
	h.virt("bond0")
	h.lower("bond0", "a")
	h.lower("bond0", "b")
	h.irq(17, "2", "")
	h.intr(17, "ata_piix", map[int]uint64{2: 5000})
	h.flushInterrupts()
	h.addr("bond0", true, "10.5.0.1")
	got := h.detect("10.5.0.1:1")
	if len(got.Vectors) != 1 || got.Vectors[0].Count != 5000 {
		t.Errorf("shared INTx: %+v", got.Vectors)
	}
}

func TestDetectBounds(t *testing.T) {
	// A multi-queue NIC with more vectors than the cap reads at most
	// nicMaxIRQsPerDev of them.
	h := newNICHost(t)
	var irqs []int
	for i := 0; i < nicMaxIRQsPerDev+50; i++ {
		irqs = append(irqs, 100+i)
	}
	h.phys("eth0", irqs...)
	h.addr("eth0", true, "10.0.0.1")
	for _, n := range irqs {
		h.irq(n, strconv.Itoa(n%12), "")
	}
	// One vector carries all the traffic; the others are listed nowhere (idle).
	// Only the first nicMaxIRQsPerDev vectors are read, the rest are not seen.
	h.intr(irqs[0], "eth0-0", map[int]uint64{irqs[0] % 12: 50000})
	h.flushInterrupts()
	got := h.detect("10.0.0.1:1")
	if len(got.Vectors) != 1 || got.Idle != nicMaxIRQsPerDev-1 {
		t.Errorf("%d vectors and %d idle, want 1 and %d (of the %d read)", len(got.Vectors), got.Idle, nicMaxIRQsPerDev-1, nicMaxIRQsPerDev)
	}
}

func TestDetectNonNumericIRQEntries(t *testing.T) {
	// A hostile or odd msi_irqs entry never reaches a path.
	h := newNICHost(t)
	h.phys("eth0", 5)
	h.write("sys/class/net/eth0/device/msi_irqs/../../../x", "x") // creates a sibling file "x", not an IRQ
	h.write("sys/class/net/eth0/device/msi_irqs/-1", "")
	h.write("sys/class/net/eth0/device/msi_irqs/0x10", "")
	h.write("sys/class/net/eth0/device/msi_irqs/12abc", "")
	h.write("sys/class/net/eth0/device/msi_irqs/99999999999999", "")
	h.irq(5, "1", "")
	h.addr("eth0", true, "10.0.0.1")
	eq(t, "IRQs", irqsOf(h.detect("10.0.0.1:1")), []int{5})
}

func FuzzListenInterfaces(f *testing.F) {
	for _, s := range []string{"192.168.50.199:8080", ":80", "[::]:80", "[::1", "[fe80::1%bond0]:1", "", ":", "[", "]:", "a:b:c", "\x00:1", "0.0.0.0:0"} {
		f.Add(s)
	}
	src := nicSource{
		sysClassNet: "/nonexistent",
		procDir:     "/nonexistent",
		ifaceAddrs: func() ([]ifaceAddr, error) {
			return []ifaceAddr{{Name: "bond0", Up: true, Addrs: []netip.Addr{netip.MustParseAddr("192.168.50.199")}}}, nil
		},
		fs: &rxCountFS{},
	}
	f.Fuzz(func(t *testing.T, addr string) {
		names, _ := listenInterfaces(addr, src)
		for _, n := range names {
			if !validIfName(n) {
				t.Fatalf("invalid name %q", n)
			}
		}
		_ = detectNICIRQs(addr, src)
	})
}

// The real host: skipped without an interface that has msi_irqs (a container,
// a VM with virtio). Where there is one, the wildcard detection must find
// vectors with servicing CPUs inside the machine, and take well under a second.
func TestDetectRealHost(t *testing.T) {
	m, _ := filepath.Glob("/sys/class/net/*/device/msi_irqs")
	if len(m) == 0 {
		t.Skip("no interface with msi_irqs on this host")
	}
	got := DetectNICIRQs(":0")
	t.Logf("ifaces %v devs %v vectors %d unknown %d reason %q took %v", got.Ifaces, got.Devs, len(got.Vectors), got.Unknown, got.Reason, got.Took)
	if got.Took > time.Second {
		t.Errorf("detection took %v", got.Took)
	}
	for _, v := range got.Vectors {
		if v.CPU < 0 || v.IRQ < 0 || v.Name == "" || v.Dev == "" {
			t.Errorf("bad vector %+v", v)
		}
	}
}

func BenchmarkDetectNICIRQsMSR1Fixture(b *testing.B) {
	h := msr1NIC(b)
	src := h.src()
	b.ResetTimer()
	for b.Loop() {
		if got := detectNICIRQs("192.168.50.199:8080", src); len(got.Vectors) != 4 {
			b.Fatal(got)
		}
	}
}
