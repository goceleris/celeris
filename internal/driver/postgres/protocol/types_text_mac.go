package protocol

import (
	"database/sql/driver"
	"fmt"
	"reflect"
)

// Type OIDs of macaddr and macaddr8.
const (
	OIDMacaddr8 uint32 = 774
	OIDMacaddr  uint32 = 829
)

func decodeMacaddrBinary(src []byte) (driver.Value, error) { return decodeMacBinary("macaddr", src, 6) }

func decodeMacaddr8Binary(src []byte) (driver.Value, error) {
	return decodeMacBinary("macaddr8", src, 8)
}

// -------------------- macaddr / macaddr8 --------------------

func decodeMacBinary(name string, src []byte, n int) (driver.Value, error) {
	if len(src) != n {
		return nil, fmt.Errorf("postgres/protocol: %s length = %d", name, len(src))
	}
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, 3*n)
	for i, b := range src {
		if i > 0 {
			buf = append(buf, ':')
		}
		buf = append(buf, hex[b>>4], hex[b&0xf])
	}
	return string(buf), nil
}

func init() {
	RegisterType(&TypeCodec{OID: OIDMacaddr, Name: "macaddr", DecodeBinary: decodeMacaddrBinary, ScanType: reflect.TypeOf("")})
	RegisterType(&TypeCodec{OID: OIDMacaddr8, Name: "macaddr8", DecodeBinary: decodeMacaddr8Binary, ScanType: reflect.TypeOf("")})
}
