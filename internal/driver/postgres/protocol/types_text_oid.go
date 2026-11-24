package protocol

import (
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"reflect"
)

// Binary-to-text codecs: the binary result form of these types is decoded into
// the text the server itself prints for the value (the output of ::text under
// the default settings). They exist so a string destination, and database/sql,
// get a value instead of an error or raw wire bytes. Only DecodeBinary is set:
// the text result format (simple protocol) is already the server's text and is
// returned untouched, and no encoder is registered (a parameter is never
// encoded through these).
//
// The text is independent of the session: the binary form carries no style, so
// an interval is always printed in the IntervalStyle "postgres" form, whatever
// the session's IntervalStyle (on the simple protocol the server prints the
// session's style and that text is returned as it is).

// OIDOid is the type OID of oid.
const OIDOid uint32 = 26

// -------------------- oid --------------------

// decodeOidBinary decodes an oid (an unsigned 32-bit number) to an int64, whose
// text is the oid's text. database/sql has no unsigned type; int64 holds the
// whole range.
func decodeOidBinary(src []byte) (driver.Value, error) {
	if len(src) != 4 {
		return nil, fmt.Errorf("postgres/protocol: oid length = %d", len(src))
	}
	return int64(binary.BigEndian.Uint32(src)), nil
}

func init() {
	RegisterType(&TypeCodec{OID: OIDOid, Name: "oid", DecodeBinary: decodeOidBinary, ScanType: reflect.TypeOf(int64(0))})
}
