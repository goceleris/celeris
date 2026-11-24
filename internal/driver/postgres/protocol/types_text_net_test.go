package protocol

import (
	"bytes"
	"testing"
)

func netBin(family, bits byte, cidr bool, addr ...byte) []byte {
	c := byte(0)
	if cidr {
		c = 1
	}
	return append([]byte{family, bits, c, byte(len(addr))}, addr...)
}

func v6(groups ...uint16) []byte {
	out := make([]byte, 0, 16)
	for _, g := range groups {
		out = append(out, byte(g>>8), byte(g))
	}
	return out
}

func TestDecodeInetCidrBinary(t *testing.T) {
	for _, c := range []struct {
		src  []byte
		cidr bool
		want string
	}{
		{netBin(2, 32, false, 10, 0, 0, 1), false, "10.0.0.1"},
		{netBin(2, 32, true, 10, 0, 0, 1), true, "10.0.0.1/32"},
		{netBin(2, 24, false, 192, 168, 1, 5), false, "192.168.1.5/24"},
		{netBin(2, 8, true, 10, 0, 0, 0), true, "10.0.0.0/8"},
		{netBin(2, 0, false, 0, 0, 0, 0), false, "0.0.0.0/0"},
		{netBin(2, 31, false, 10, 0, 0, 1), false, "10.0.0.1/31"},
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0, 0, 1)...), false, "::1"},
		{netBin(3, 128, true, v6(0, 0, 0, 0, 0, 0, 0, 1)...), true, "::1/128"},
		{netBin(3, 0, true, v6(0, 0, 0, 0, 0, 0, 0, 0)...), true, "::/0"},
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0, 0, 0)...), false, "::"},
		{netBin(3, 127, false, v6(0, 0, 0, 0, 0, 0, 0, 1)...), false, "::1/127"},
		{netBin(3, 32, true, v6(0x2001, 0xdb8, 0, 0, 0, 0, 0, 0)...), true, "2001:db8::/32"},
		{netBin(3, 128, false, v6(1, 0, 0, 0, 0, 0, 0, 0)...), false, "1::"},
		{netBin(3, 128, false, v6(1, 2, 3, 4, 5, 6, 7, 8)...), false, "1:2:3:4:5:6:7:8"},
		// a run of one zero group is not collapsed; the first of equal runs is
		{netBin(3, 128, false, v6(1, 0, 2, 3, 4, 5, 6, 7)...), false, "1:0:2:3:4:5:6:7"},
		{netBin(3, 128, false, v6(1, 0, 0, 2, 0, 0, 3, 4)...), false, "1::2:0:0:3:4"},
		{netBin(3, 128, false, v6(1, 0, 0, 2, 0, 0, 0, 3)...), false, "1:0:0:2::3"},
		// IPv4 tails: compatible and mapped
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0, 0x0102, 0x0304)...), false, "::1.2.3.4"},
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0xffff, 0x0102, 0x0304)...), false, "::ffff:1.2.3.4"},
		{netBin(3, 96, false, v6(0, 0, 0, 0, 0, 0xffff, 0, 0)...), false, "::ffff:0.0.0.0/96"},
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0, 0, 2)...), false, "::2"},
		{netBin(3, 128, false, v6(0, 0, 0, 0, 0, 0, 0, 0x0102)...), false, "::102"},
	} {
		var v any
		var err error
		if c.cidr {
			v, err = decodeCidrBinary(c.src)
		} else {
			v, err = decodeInetBinary(c.src)
		}
		if err != nil || v != c.want {
			t.Errorf("% x (cidr %v): got %v err %v, want %q", c.src, c.cidr, v, err, c.want)
		}
	}
	for _, bad := range [][]byte{
		nil, {2, 32, 0}, netBin(2, 33, false, 1, 2, 3, 4), netBin(3, 129, false, v6(0, 0, 0, 0, 0, 0, 0, 1)...),
		netBin(4, 8, false, 1, 2, 3, 4), netBin(2, 8, false, 1, 2, 3), append(netBin(2, 8, false, 1, 2, 3, 4), 0),
	} {
		if v, err := decodeInetBinary(bad); err == nil {
			t.Errorf("% x: got %v, want an error", bad, v)
		}
	}
	if !bytes.Equal(v6(1, 2), []byte{0, 1, 0, 2}) {
		t.Fatal("v6 helper")
	}
}
