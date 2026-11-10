//go:build linux

package platform

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"slices"
	"syscall"
	"testing"
)

// The parser of the RTM_GETADDR dump (celeris#885 item 53), against dumps built
// byte by byte: the host's own dump (TestOSIfaceAddrsIsOneAddressDump885) has
// no point-to-point link, no mapped address and no truncated message.

// attr885 is one rtattr: len, type, payload, padded to 4 bytes.
func attr885(typ uint16, payload []byte) []byte {
	b := make([]byte, 4, 4+len(payload)+3)
	binary.NativeEndian.PutUint16(b[0:], uint16(4+len(payload)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	b = append(b, payload...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// msg885 is one netlink message of a dump: a header and data.
func msg885(typ uint16, data []byte) []byte {
	n := syscall.NLMSG_HDRLEN + len(data)
	b := make([]byte, syscall.NLMSG_HDRLEN, (n+3)&^3)
	binary.NativeEndian.PutUint32(b[0:], uint32(n))
	binary.NativeEndian.PutUint16(b[4:], typ)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// newaddr885 is an RTM_NEWADDR message for the interface index.
func newaddr885(family byte, index uint32, attrs ...[]byte) []byte {
	d := make([]byte, syscall.SizeofIfAddrmsg)
	d[0] = family
	d[1] = 24
	binary.NativeEndian.PutUint32(d[4:], index)
	for _, a := range attrs {
		d = append(d, a...)
	}
	return msg885(syscall.RTM_NEWADDR, d)
}

func ip885(s string) []byte {
	a := netip.MustParseAddr(s)
	return a.AsSlice()
}

func done885() []byte { return msg885(syscall.NLMSG_DONE, make([]byte, 4)) }

func cat885(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// unpadded885 is an RTM_NEWADDR whose last attribute carries n payload bytes
// and is not padded to 4, with the message length counting exactly its bytes.
func unpadded885(n int) []byte {
	d := make([]byte, syscall.SizeofIfAddrmsg)
	d[0] = syscall.AF_INET
	binary.NativeEndian.PutUint32(d[4:], 2)
	d = append(d, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1"))...)
	last := make([]byte, 4+n)
	binary.NativeEndian.PutUint16(last[0:], uint16(4+n))
	binary.NativeEndian.PutUint16(last[2:], syscall.IFA_LABEL)
	d = append(d, last...)
	return msg885(syscall.RTM_NEWADDR, d)
}

// viaDump885 reads rib the way Listen does: as the dump netlinkRIB returned.
func viaDump885(rib []byte) (map[int][]netip.Addr, bool) {
	old := netlinkRIB
	netlinkRIB = func(int, int) ([]byte, error) { return rib, nil }
	defer func() { netlinkRIB = old }()
	return addrsByIndex()
}

// TestOSIfaceAddrsFallsBackOnAnUnparseableDump885: a dump the parser cannot
// read gives the table the per-interface calls give, and does not panic.
func TestOSIfaceAddrsFallsBackOnAnUnparseableDump885(t *testing.T) {
	want, err := refIfaceAddrs885()
	if err != nil {
		t.Skipf("net.Interfaces: %v", err)
	}
	old := netlinkRIB
	t.Cleanup(func() { netlinkRIB = old })
	netlinkRIB = func(int, int) ([]byte, error) { return unpadded885(5), nil }
	got, err := osIfaceAddrs()
	if err != nil {
		t.Fatalf("osIfaceAddrs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallback table differs from the per-interface one:\n got  %+v\n want %+v", got, want)
	}
}

func TestParseAddrRIB885(t *testing.T) {
	a := netip.MustParseAddr
	for _, tc := range []struct {
		name string
		rib  []byte
		want map[int][]netip.Addr
		ok   bool
	}{
		{"ipv4", cat885(newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1")), attr885(syscall.IFA_LOCAL, ip885("10.0.0.1"))), done885()),
			map[int][]netip.Addr{2: {a("10.0.0.1")}}, true},
		// A point-to-point link: IFA_ADDRESS is the peer, IFA_LOCAL is ours, in
		// either order.
		{"p2p-address-first", cat885(newaddr885(syscall.AF_INET, 5, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.9")), attr885(syscall.IFA_LOCAL, ip885("10.0.0.1"))), done885()),
			map[int][]netip.Addr{5: {a("10.0.0.1")}}, true},
		{"p2p-local-first", cat885(newaddr885(syscall.AF_INET, 5, attr885(syscall.IFA_LOCAL, ip885("10.0.0.1")), attr885(syscall.IFA_ADDRESS, ip885("10.0.0.9"))), done885()),
			map[int][]netip.Addr{5: {a("10.0.0.1")}}, true},
		// Only IFA_ADDRESS (every IPv6 address, an unnumbered IPv4 link).
		{"address-only-v6", cat885(newaddr885(syscall.AF_INET6, 3, attr885(syscall.IFA_ADDRESS, ip885("2001:db8::1"))), done885()),
			map[int][]netip.Addr{3: {a("2001:db8::1")}}, true},
		// Order is the kernel's, per interface, across interleaved interfaces.
		{"interleaved", cat885(
			newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1"))),
			newaddr885(syscall.AF_INET, 3, attr885(syscall.IFA_ADDRESS, ip885("10.1.0.1"))),
			newaddr885(syscall.AF_INET6, 2, attr885(syscall.IFA_ADDRESS, ip885("fe80::1"))),
			newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.2"))), done885()),
			map[int][]netip.Addr{2: {a("10.0.0.1"), a("fe80::1"), a("10.0.0.2")}, 3: {a("10.1.0.1")}}, true},
		// An IPv4-mapped IPv6 address is unmapped, as the table did.
		{"mapped", cat885(newaddr885(syscall.AF_INET6, 4, attr885(syscall.IFA_ADDRESS, ip885("::ffff:1.2.3.4"))), done885()),
			map[int][]netip.Addr{4: {a("1.2.3.4")}}, true},
		// Not an IP family, a payload too short for its family, no address
		// attribute: nothing for that message, and no failure.
		{"other-family", cat885(newaddr885(syscall.AF_PACKET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1"))), done885()), map[int][]netip.Addr{}, true},
		{"short-payload", cat885(newaddr885(syscall.AF_INET6, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1"))), done885()), map[int][]netip.Addr{}, true},
		{"no-address-attribute", cat885(newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_LABEL, []byte("eth0\x00"))), done885()), map[int][]netip.Addr{}, true},
		// Messages of another type are skipped; the dump ends at NLMSG_DONE.
		{"foreign-type-and-after-done", cat885(msg885(syscall.RTM_NEWLINK, make([]byte, 16)),
			newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1"))), done885(),
			newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.2")))),
			map[int][]netip.Addr{2: {a("10.0.0.1")}}, true},
		{"empty", nil, map[int][]netip.Addr{}, true},
		// A message that cannot be read makes the whole table fall back.
		{"short-ifaddrmsg", cat885(msg885(syscall.RTM_NEWADDR, make([]byte, 4)), done885()), nil, false},
		{"length-past-the-buffer", func() []byte {
			m := newaddr885(syscall.AF_INET, 2, attr885(syscall.IFA_ADDRESS, ip885("10.0.0.1")))
			binary.NativeEndian.PutUint32(m[0:], 4096)
			return m
		}(), nil, false},
		// A final attribute whose length is not a multiple of 4 and that has no
		// padding: the std function that walks the attributes slices past the end
		// of the message and panics. A kernel dump pads every attribute, so this
		// is not a dump the kernel makes, but the table is built at Listen and
		// must fall back, not panic (review of #1127, minor 1). The first row is
		// the fuzz crasher of that review.
		{"unpadded-final-attribute-fuzz-crasher", []byte("1\x00\x00\x00\x14\x00000000000000000000\b\x00000000\b\x00000000\t\x000000000000"), nil, false},
		{"unpadded-final-attribute-1", unpadded885(1), nil, false},
		{"unpadded-final-attribute-2", unpadded885(2), nil, false},
		{"unpadded-final-attribute-3", unpadded885(3), nil, false},
		{"unpadded-final-attribute-5", unpadded885(5), nil, false},
		{"unpadded-final-attribute-6", unpadded885(6), nil, false},
		{"unpadded-final-attribute-7", unpadded885(7), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := viaDump885(tc.rib)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAddrRIB = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHostDumpParses885: the host's own RTM_GETADDR dump is read and parsed (the
// table is not made the old way, one Addrs call per interface, which the
// equivalence test would also accept), and holds loopback's 127.0.0.1.
func TestHostDumpParses885(t *testing.T) {
	byIndex, ok := addrsByIndex()
	if !ok {
		t.Fatal("the host's address dump could not be read or parsed: osIfaceAddrs falls back to one dump per interface")
	}
	found := false
	for _, addrs := range byIndex {
		if slices.Contains(addrs, netip.MustParseAddr("127.0.0.1")) {
			found = true
		}
	}
	if !found {
		t.Errorf("127.0.0.1 is in no interface's addresses: %v", byIndex)
	}
}
