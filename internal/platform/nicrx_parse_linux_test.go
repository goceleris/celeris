//go:build linux

package platform

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// The parsers of the rx-port rule. Adapted from celeris#1062's detection tests
// (nicirq_linux_test.go on fix/irqplace-1036), plus the rx_packets parser.

func TestParseRxPackets(t *testing.T) {
	for in, want := range map[string]uint64{
		"0\n": 0, "1": 1, "80891505972\n": 80891505972, " 42 \n": 42,
		"18446744073709551615\n": ^uint64(0),
	} {
		if got, ok := parseRxPackets([]byte(in)); !ok || got != want {
			t.Errorf("parseRxPackets(%q) = %d,%v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "\n", "-1", "+1", "0x10", "12 34", "1e9", "abc", "12\n34\n", "18446744073709551616", "999999999999999999999", "１２", "\x00"} {
		if got, ok := parseRxPackets([]byte(in)); ok {
			t.Errorf("parseRxPackets(%q) = %d, true, want false", in, got)
		}
	}
}

func TestParseSingleCPU(t *testing.T) {
	for in, want := range map[string]int{"4": 4, "4\n": 4, " 12 ": 12, "4-4": 4, "0": 0} {
		if got, ok := parseSingleCPU(in); !ok || got != want {
			t.Errorf("parseSingleCPU(%q) = %d,%v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "\n", "4-5", "4,6", "0-11", "-1", "4-", "-4", "a", "4 5", "4--4", "1e3", "+4", "70000"} {
		if got, ok := parseSingleCPU(in); ok {
			t.Errorf("parseSingleCPU(%q) = %d, true, want false", in, got)
		}
	}
}

func TestActionMatchesIface(t *testing.T) {
	for _, tc := range []struct {
		action, iface string
		want          bool
	}{
		{"eth0", "eth0", true},
		{"eth0-TxRx-0", "eth0", true},
		{"eth0:tx-0", "eth0", true},
		{"eth0@pci", "eth0", true},
		{"eth0_q1", "eth0", true},
		{"eth10-0", "eth1", false},
		{"eth1x", "eth1", false},
		{"xeth1-0", "eth1", false},
		{"eth", "eth1", false},
		{"", "eth1", false},
	} {
		if got := actionMatchesIface(tc.action, tc.iface); got != tc.want {
			t.Errorf("actionMatchesIface(%q, %q) = %v, want %v", tc.action, tc.iface, got, tc.want)
		}
	}
}

func TestValidIfName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "a\x00b", strings.Repeat("a", 65)} {
		if validIfName(bad) {
			t.Errorf("validIfName(%q) = true", bad)
		}
	}
	for _, ok := range []string{"eth0", "enp1s0", "bond0.100", "br-1a2b", "lo"} {
		if !validIfName(ok) {
			t.Errorf("validIfName(%q) = false", ok)
		}
	}
	// An interface the table lists under an unusable name is skipped, and one
	// that names a parent directory never builds a path.
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	h.ifaces = append([]ifaceAddr{{Name: "../../etc", Up: true, Addrs: []netip.Addr{netip.MustParseAddr("192.168.50.199")}}}, h.ifaces...)
	names, _ := listenInterfaces(bondAddr, h.nic())
	eq(t, "ifaces", names, []string{"bond0"})
}

func TestListenInterfaces(t *testing.T) {
	h := msr1RxReal(t, "msr1_rxfree_vector_on_cpu1.txt")
	h.addr("eth9", true)                 // up, no address
	h.addr("link0", true, "fe80::42")    // link-local only
	h.addr("down0", false, "172.16.0.2") // down
	for _, tc := range []struct {
		addr string
		want []string
		why  string
	}{
		{"192.168.50.199:8080", []string{"bond0"}, ""},
		{"[::ffff:192.168.50.199]:80", []string{"bond0"}, ""},
		{"[fe80::a6e4:7cff:fe79:9662%bond0]:80", []string{"bond0"}, ""},
		{"127.0.0.1:8080", nil, "loopback"},
		{"[::1]:8080", nil, "loopback"},
		{"10.9.9.9:80", nil, "no interface owns"},
		{"localhost:8080", nil, "not an IP literal"},
		{"[::1:80", nil, "not host:port"},
		// A wildcard: every up, non-loopback interface with a global address.
		{":8080", []string{"bond0", "tailscale0", "docker0"}, ""},
		{"0.0.0.0:0", []string{"bond0", "tailscale0", "docker0"}, ""},
		{"[::]:8080", []string{"bond0", "tailscale0", "docker0"}, ""},
	} {
		names, why := listenInterfaces(tc.addr, h.nic())
		slices.Sort(names)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		eq(t, tc.addr, names, want)
		if tc.why != "" && !strings.Contains(why, tc.why) {
			t.Errorf("%s: reason %q, want it to say %q", tc.addr, why, tc.why)
		}
	}
	// Only the physical ports behind them count: the bond's slaves, not
	// tailscale0 or docker0, which have no device.
	devs := lowerDevices([]string{"bond0", "tailscale0", "docker0"}, h.nic())
	eq(t, "devs", devs, []string{"enp1s0", "enp49s0"})
}

func TestLowerDevicesCycleAndDepth(t *testing.T) {
	h := newRxHost(t)
	h.phys("eth0", 1)
	h.virt("a")
	h.virt("b")
	h.lower("a", "b")
	h.lower("b", "a") // a cycle
	h.lower("b", "eth0")
	eq(t, "cycle", lowerDevices([]string{"a"}, h.nic()), []string{"eth0"})
	// vlan -> bond -> slave is two levels; more than nicMaxDepth is not followed.
	prev := "eth0"
	for i := 0; i < nicMaxDepth+2; i++ {
		name := "v" + string(rune('a'+i))
		h.virt(name)
		h.lower(name, prev)
		prev = name
	}
	if got := lowerDevices([]string{prev}, h.nic()); len(got) != 0 {
		t.Errorf("a chain deeper than %d levels reached %v", nicMaxDepth, got)
	}
}

func TestParseInterrupts(t *testing.T) {
	const file = `           CPU0       CPU1       CPU3
  3:       100        200        300     GICv3  27 Level     arch_timer
127:         0          0       5000  ITS-MSI 524288 Edge      enp49s0-0
128:         1          2          3  PCI-MSIX-0000:01:00.0 0-edge      eth0, eth1
129:         1          2    ITS-MSI 1 Edge x
130:         1          2          x  ITS-MSI 1 Edge y
131:         1          2  99999999999999999999999  ITS-MSI 1 Edge z
IPI0:         1          2          3  Rescheduling interrupts
Err:          0
 abc:         1          2          3  Edge  q
132:         7          0          0
133:         1          2
`
	rows := parseInterrupts([]byte(file))
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (3, 127, 128, 132): %+v", len(rows), rows)
	}
	eq(t, "row 3", rows[0].irq, 3)
	eq(t, "row 3 total", rows[0].total, uint64(600))
	eq(t, "row 3 label", rows[0].label, "arch_timer")
	eq(t, "row 127 cpus", rows[1].cpus, []int{0, 1, 3})
	eq(t, "row 127 counts", rows[1].counts, []uint64{0, 0, 5000})
	eq(t, "row 127 label", rows[1].label, "enp49s0-0")
	eq(t, "row 128 names", rows[2].names, []string{"PCI-MSIX-0000:01:00.0", "0-edge", "eth0", "eth1"})
	eq(t, "row 128 label", rows[2].label, "eth1")
	eq(t, "row 132 no label", rows[3].label, "")

	for name, in := range map[string]string{
		"empty":           "",
		"blank":           "\n\n",
		"header only":     "      CPU0   CPU1\n",
		"no CPU header":   "  hello world\n127: 1 2 x\n",
		"bad header cpu":  "  CPU0 CPUx\n127: 1 2 x\n",
		"header trailing": "  CPU0 CPU1 foo\n127: 1 2 x\n",
	} {
		if got := parseInterrupts([]byte(in)); len(got) != 0 {
			t.Errorf("%s: got %d rows, want none", name, len(got))
		}
	}
	// An overflowing total saturates and never wraps.
	r := parseInterrupts([]byte("CPU0 CPU1\n5: 18446744073709551615 18446744073709551615 x\n"))
	if len(r) != 1 || r[0].total != ^uint64(0) {
		t.Errorf("overflow: %+v", r)
	}
	// Offline CPUs have no column: columns follow the header, not the position.
	h := parseInterrupts([]byte("CPU0 CPU2 CPU5\n9: 1 2 3 x y\n"))
	eq(t, "header columns", h[0].cpus, []int{0, 2, 5})
}

func TestEffectiveCPUSources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eff, smp string // "-" leaves the file out
		want     int
	}{
		{"effective names one CPU", "6", "0-11", 6},
		{"effective wins over smp", "6", "4", 6},
		{"effective spans CPUs, smp names one", "0-11", "4", 4},
		{"effective absent, smp names one", "-", "4", 4},
		{"effective empty, smp names one", "", "4", 4},
		{"both span CPUs", "0-11", "0-11", -1},
		{"no files", "-", "-", -1},
		{"effective garbage", "x", "4", -1},
		{"smp garbage", "-", "x", -1},
	} {
		h := newRxHost(t)
		if tc.eff != "-" {
			h.write("proc/irq/9/effective_affinity_list", tc.eff+"\n")
		}
		if tc.smp != "-" {
			h.write("proc/irq/9/smp_affinity_list", tc.smp+"\n")
		}
		if got := effectiveCPU(9, h.nic()); got != tc.want {
			t.Errorf("%s: effectiveCPU = %d, want %d", tc.name, got, tc.want)
		}
	}
	// A read error is not worked around: effective unreadable, smp readable.
	h := newRxHost(t)
	h.irq(9, "6", "4")
	h.fs.deny = func(n string) bool { return strings.HasSuffix(n, "effective_affinity_list") }
	if got := effectiveCPU(9, h.nic()); got != -1 {
		t.Errorf("unreadable effective list fell back to %d", got)
	}
}

func TestAtLeastPercentExact(t *testing.T) {
	for _, tc := range []struct {
		n, total, pct uint64
		want          bool
	}{
		{60, 100, 60, true}, {59, 100, 60, false}, {0, 0, 60, true}, {1, 0, 60, true},
		{^uint64(0), ^uint64(0), 60, true}, {^uint64(0) / 2, ^uint64(0), 60, false},
		{^uint64(0) / 10 * 6, ^uint64(0), 60, false}, {^uint64(0)/10*6 + 3, ^uint64(0), 60, true},
	} {
		if got := atLeastPercent(tc.n, tc.total, tc.pct); got != tc.want {
			t.Errorf("atLeastPercent(%d, %d, %d) = %v, want %v", tc.n, tc.total, tc.pct, got, tc.want)
		}
	}
	if satAdd(^uint64(0), 5) != ^uint64(0) || satAdd(2, 3) != 5 {
		t.Error("satAdd")
	}
}

func FuzzParseInterrupts(f *testing.F) {
	f.Add([]byte("           CPU0       CPU1\n127: 1 2 ITS-MSI 1 Edge eth0\nIPI0: 1 2 x\n"))
	f.Add([]byte("CPU0\n1: 5 a, b\n"))
	f.Add([]byte(""))
	f.Add([]byte("CPU0 CPU1 CPU99999999999\n1: 1 2\n"))
	f.Add([]byte("\x00\xff:\n::\n:::"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, r := range parseInterrupts(b) {
			if len(r.counts) != len(r.cpus) || r.irq < 0 {
				t.Fatalf("bad row %+v", r)
			}
		}
	})
}

func FuzzParseRxPackets(f *testing.F) {
	for _, s := range []string{"0\n", "80891505972\n", "-1", "", "18446744073709551616", "1 2", "\x00", "９"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		n, ok := parseRxPackets(b)
		if !ok {
			return
		}
		// What parsed is exactly one decimal number with optional white space.
		s := strings.TrimSpace(string(b))
		for _, c := range s {
			if c < '0' || c > '9' {
				t.Fatalf("parseRxPackets(%q) = %d accepted a non-digit", b, n)
			}
		}
	})
}

func FuzzNICParsers(f *testing.F) {
	for _, s := range []string{"4", "4-4", "0-11", "4,6", "", "-", "--", "99999999999999999999", "eth0-1", ":", "\x00"} {
		f.Add(s, "eth0")
	}
	f.Fuzz(func(t *testing.T, s, iface string) {
		if c, ok := parseSingleCPU(s); ok && (c < 0 || c >= nicMaxCPUs) {
			t.Fatalf("parseSingleCPU(%q) = %d", s, c)
		}
		if n, ok := parseIRQNumber(s); ok && (n < 0 || n > nicMaxIRQ) {
			t.Fatalf("parseIRQNumber(%q) = %d", s, n)
		}
		if isCPUList(s) && strings.TrimSpace(s) == "" {
			t.Fatalf("isCPUList(%q)", s)
		}
		_ = actionMatchesIface(s, iface)
		_ = validIfName(s)
	})
}

// FuzzRxFreeAddr feeds arbitrary listen addresses to the whole decision over
// the real msr1 snapshot: it must not panic, and anything it frees is a big CPU.
func FuzzRxFreeAddr(f *testing.F) {
	for _, s := range []string{"192.168.50.199:8080", ":80", "[::]:80", "[::1", "[fe80::1%bond0]:1", "", ":", "[", "]:", "a:b:c", "\x00:1", "0.0.0.0:0"} {
		f.Add(s)
	}
	h := msr1RxReal(f, "msr1_rxfree_vector_on_cpu1.txt")
	src := h.nic()
	big := []int{0, 1, 6, 7, 8, 9, 10, 11}
	f.Fuzz(func(t *testing.T, addr string) {
		names, _ := listenInterfaces(addr, src)
		for _, n := range names {
			if !validIfName(n) {
				t.Fatalf("invalid name %q", n)
			}
		}
		rf := rxFreeCPUs(addr, big, src)
		for _, c := range rf.Freed {
			if !slices.Contains(big, c) {
				t.Fatalf("freed CPU %d is not a big CPU", c)
			}
		}
		if len(rf.Freed) > rxFreeBudget(len(big)) {
			t.Fatalf("freed %v, over the budget", rf.Freed)
		}
	})
}
