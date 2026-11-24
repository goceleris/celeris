package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
)

// Type OIDs of inet and cidr.
const (
	OIDCidr uint32 = 650
	OIDInet uint32 = 869
)

// -------------------- inet / cidr --------------------

const (
	pgAFInet  = 2 // AF_INET
	pgAFInet6 = 3 // PGSQL_AF_INET6, not the platform's AF_INET6
)

func decodeInetBinary(src []byte) (driver.Value, error) { return decodeNetworkBinary(src, false) }
func decodeCidrBinary(src []byte) (driver.Value, error) { return decodeNetworkBinary(src, true) }

// decodeNetworkBinary decodes an inet or cidr value: family, mask bits, an
// is-cidr flag, the address length and the address. inet prints the mask unless
// it is the full width (/32, /128); cidr always prints it.
func decodeNetworkBinary(src []byte, cidr bool) (driver.Value, error) {
	name := "inet"
	if cidr {
		name = "cidr"
	}
	if len(src) < 4 {
		return nil, fmt.Errorf("postgres/protocol: %s length = %d", name, len(src))
	}
	family, bits, nb := src[0], int(src[1]), int(src[3])
	if len(src) != 4+nb {
		return nil, fmt.Errorf("postgres/protocol: %s length = %d, address length byte = %d", name, len(src), nb)
	}
	addr := src[4:]
	buf := make([]byte, 0, 48)
	var maxBits int
	switch {
	case family == pgAFInet && nb == 4:
		maxBits = 32
		if bits > maxBits {
			return nil, fmt.Errorf("postgres/protocol: %s mask %d out of range", name, bits)
		}
		buf = appendIPv4(buf, addr)
	case family == pgAFInet6 && nb == 16:
		maxBits = 128
		if bits > maxBits {
			return nil, fmt.Errorf("postgres/protocol: %s mask %d out of range", name, bits)
		}
		buf = appendIPv6(buf, addr)
	default:
		return nil, fmt.Errorf("postgres/protocol: %s: unsupported family %d with %d address bytes", name, family, nb)
	}
	if cidr || bits != maxBits {
		buf = append(buf, '/')
		buf = strconv.AppendInt(buf, int64(bits), 10)
	}
	return string(buf), nil
}

func appendIPv4(dst, a []byte) []byte {
	for i, b := range a {
		if i > 0 {
			dst = append(dst, '.')
		}
		dst = strconv.AppendInt(dst, int64(b), 10)
	}
	return dst
}

// appendIPv6 prints a 16-byte address as the server does (inet_net_ntop): hex
// groups without leading zeros, the first longest run of two or more zero
// groups collapsed to "::", and an IPv4 tail for the compatible (::a.b.c.d) and
// mapped (::ffff:a.b.c.d) forms.
func appendIPv6(dst, a []byte) []byte {
	var w [8]uint16
	for i := range w {
		w[i] = binary.BigEndian.Uint16(a[2*i:])
	}
	bestBase, bestLen := -1, 0
	curBase, curLen := -1, 0
	for i := 0; i < 8; i++ {
		if w[i] == 0 {
			if curBase == -1 {
				curBase, curLen = i, 1
			} else {
				curLen++
			}
		} else if curBase != -1 {
			if bestBase == -1 || curLen > bestLen {
				bestBase, bestLen = curBase, curLen
			}
			curBase = -1
		}
	}
	if curBase != -1 && (bestBase == -1 || curLen > bestLen) {
		bestBase, bestLen = curBase, curLen
	}
	if bestBase != -1 && bestLen < 2 {
		bestBase = -1
	}
	for i := 0; i < 8; i++ {
		if bestBase != -1 && i >= bestBase && i < bestBase+bestLen {
			if i == bestBase {
				dst = append(dst, ':')
			}
			continue
		}
		if i != 0 {
			dst = append(dst, ':')
		}
		if i == 6 && bestBase == 0 && (bestLen == 6 || (bestLen == 7 && w[7] != 1) || (bestLen == 5 && w[5] == 0xffff)) {
			return appendIPv4(dst, a[12:16])
		}
		dst = strconv.AppendUint(dst, uint64(w[i]), 16)
	}
	if bestBase != -1 && bestBase+bestLen == 8 {
		dst = append(dst, ':')
	}
	return dst
}

func init() {
	RegisterType(&TypeCodec{OID: OIDInet, Name: "inet", DecodeBinary: decodeInetBinary, ScanType: reflect.TypeOf("")})
	RegisterType(&TypeCodec{OID: OIDCidr, Name: "cidr", DecodeBinary: decodeCidrBinary, ScanType: reflect.TypeOf("")})
}
