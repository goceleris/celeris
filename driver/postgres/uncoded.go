package postgres

import "fmt"

// binaryOnlyTypes names the PostgreSQL types the driver has no codec for and
// whose BINARY wire form is not their text: the extended protocol returns them
// in binary, so a string or []byte destination would receive raw wire bytes
// (an interval is 16 bytes, an inet a family and mask byte plus the address, an
// array a header and length-prefixed elements). Scanning one into *string or
// *[]byte is an error that names the type and says to cast it to text in the
// query, rather than a value that only looks like data. *any and a
// sql.Scanner still receive the raw bytes, as in v1.5.11.
//
// A type that is not listed (an enum, a domain over text, a composite, a range
// or extension type the driver has never heard of) is left as it was: for text,
// varchar, bpchar, name, xml and enum the binary form IS the text, and the
// driver cannot tell an enum from an unknown type by its OID.
var binaryOnlyTypes = map[uint32]string{
	26: "oid", 27: "tid", 28: "xid", 29: "cid",
	600: "point", 601: "lseg", 602: "path", 603: "box", 604: "polygon", 628: "line", 718: "circle",
	650: "cidr", 774: "macaddr8", 790: "money", 829: "macaddr", 869: "inet",
	1083: "time", 1186: "interval", 1266: "timetz",
	1560: "bit", 1562: "varbit",
	3614: "tsvector", 3615: "tsquery",
	3904: "int4range", 3906: "numrange", 3908: "tsrange", 3910: "tstzrange", 3912: "daterange", 3926: "int8range",
	// arrays of the above and of the other types without an array codec
	1014: "bpchar[]", 1017: "point[]", 1028: "oid[]", 1040: "macaddr[]", 1041: "inet[]",
	1115: "timestamp[]", 1182: "date[]", 1183: "time[]", 1185: "timestamptz[]", 1187: "interval[]",
	1231: "numeric[]", 1270: "timetz[]", 651: "cidr[]", 1561: "bit[]", 1563: "varbit[]",
}

// errBinaryOnly is the error for scanning a column of a binary-only type with
// no codec into a string or []byte.
func errBinaryOnly(oid uint32, name string) error {
	return fmt.Errorf("celeris-postgres: scan: column of type %s (OID %d) has no decoder in this driver, so a string or []byte "+
		"would be its raw binary wire form; cast it to text in the query (SELECT col::text) or scan into any", name, oid)
}
