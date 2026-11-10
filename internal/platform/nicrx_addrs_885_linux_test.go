//go:build linux

package platform

import (
	"net"
	"net/netip"
	"reflect"
	"syscall"
	"testing"
)

// celeris#885 item 53. osIfaceAddrs built the interface table of the NIC
// interrupt detection (PR #1062) with net.Interfaces and then
// (*Interface).Addrs for each of up to 512 interfaces, and every Addrs call is
// a full RTM_GETADDR dump of all addresses of the host, down and loopback
// interfaces included. A hostNetwork node with hundreds of veths paid hundreds
// of dumps, at start and again at the adaptive engine's standby Listen. The
// table is now one RTM_GETADDR dump, mapped by interface index.

// refIfaceAddrs885 is osIfaceAddrs as it was (one Addrs call per interface),
// the oracle for the table.
func refIfaceAddrs885() ([]ifaceAddr, error) {
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

// TestOSIfaceAddrsIsOneAddressDump885: the table equals the one the
// per-interface calls give, and costs one RTM_GETADDR dump however many
// interfaces the host has.
func TestOSIfaceAddrsIsOneAddressDump885(t *testing.T) {
	want, err := refIfaceAddrs885()
	if err != nil {
		t.Skipf("net.Interfaces: %v", err)
	}
	var dumps int
	old := netlinkRIB
	t.Cleanup(func() { netlinkRIB = old })
	netlinkRIB = func(proto, family int) ([]byte, error) {
		if proto == syscall.RTM_GETADDR {
			dumps++
		}
		return old(proto, family)
	}
	got, err := osIfaceAddrs()
	if err != nil {
		t.Fatalf("osIfaceAddrs: %v", err)
	}
	if dumps != 1 {
		t.Errorf("osIfaceAddrs made %d RTM_GETADDR dumps through netlinkRIB for %d interfaces, want 1 (net.Interface.Addrs, one dump per interface, does not use it)", dumps, len(got))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("table differs from the per-interface one:\n got  %+v\n want %+v", got, want)
	}
}

// BenchmarkOSIfaceAddrs885 is the table's cost on this host: the figure moves
// with the interface count (a lane's container gets a few hundred dummy links).
func BenchmarkOSIfaceAddrs885(b *testing.B) {
	for b.Loop() {
		if _, err := osIfaceAddrs(); err != nil {
			b.Fatal(err)
		}
	}
	ifs, _ := net.Interfaces()
	b.ReportMetric(float64(len(ifs)), "ifaces")
}

// BenchmarkRefIfaceAddrs885 is the same for the per-interface calls this
// replaces.
func BenchmarkRefIfaceAddrs885(b *testing.B) {
	for b.Loop() {
		if _, err := refIfaceAddrs885(); err != nil {
			b.Fatal(err)
		}
	}
	ifs, _ := net.Interfaces()
	b.ReportMetric(float64(len(ifs)), "ifaces")
}
