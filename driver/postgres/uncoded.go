package postgres

import (
	"fmt"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// binaryOnlyTypes names the PostgreSQL built-in types the driver has no codec
// for and whose BINARY wire form is not their text: the extended protocol
// returns them in binary, so a string or []byte destination would receive raw
// wire bytes (an interval is 16 bytes, an inet a family and mask byte plus the
// address, an array a header and length-prefixed elements). Scanning one into
// *string or *[]byte is an error that names the type and says to cast it to text
// in the query, rather than a value that only looks like data. *any and a
// sql.Scanner still receive the raw bytes, as in v1.5.11.
//
// The list is every built-in base, range and multirange type whose send
// function is not a text one (textsend, varcharsend, bpcharsend, namesend,
// xml_send, charsend, enum_send ...) and that has no codec here, plus the arrays
// of such types and of the types with a text send but no array codec, taken from
// pg_type of PostgreSQL 18, checked against PostgreSQL 14 (only OIDs below
// 10000, which are fixed in the source; the information_schema array types at
// 13000 and up get a different OID in each major version and are not listed),
// and record / record[]. A type that is not listed (an
// enum, a named composite, a range or extension type the driver has never heard
// of: OIDs from 16384 up) is left as it was: for text, varchar, bpchar, name,
// xml and enum the binary form IS the text, and the driver cannot tell an enum
// from an unknown type by its OID.
var binaryOnlyTypes = map[uint32]string{
	22: "int2vector", 24: "regproc", 27: "tid",
	28: "xid", 29: "cid", 30: "oidvector", 143: "xml[]",
	271: "xid8[]", 600: "point", 601: "lseg", 602: "path",
	603: "box", 604: "polygon", 628: "line", 629: "line[]",
	650: "cidr", 651: "cidr[]", 718: "circle", 719: "circle[]",
	774: "macaddr8", 775: "macaddr8[]", 790: "money", 791: "money[]",
	829: "macaddr", 869: "inet", 1002: "char[]", 1003: "name[]",
	1006: "int2vector[]", 1008: "regproc[]", 1010: "tid[]", 1011: "xid[]",
	1012: "cid[]", 1013: "oidvector[]", 1014: "bpchar[]", 1017: "point[]",
	1018: "lseg[]", 1019: "path[]", 1020: "box[]", 1027: "polygon[]",
	1028: "oid[]", 1034: "aclitem[]", 1040: "macaddr[]", 1041: "inet[]",
	1115: "timestamp[]", 1182: "date[]", 1183: "time[]",
	1185: "timestamptz[]", 1187: "interval[]", 1231: "numeric[]",
	1270: "timetz[]", 1560: "bit", 1561: "bit[]",
	1562: "varbit", 1563: "varbit[]", 2201: "refcursor[]", 2202: "regprocedure",
	2203: "regoper", 2204: "regoperator", 2205: "regclass", 2206: "regtype",
	2207: "regprocedure[]", 2208: "regoper[]", 2209: "regoperator[]", 2210: "regclass[]",
	2211: "regtype[]", 2249: "record", 2287: "record[]", 2949: "txid_snapshot[]",
	3220: "pg_lsn", 3221: "pg_lsn[]", 3614: "tsvector", 3615: "tsquery",
	3643: "tsvector[]", 3644: "gtsvector[]", 3645: "tsquery[]", 3734: "regconfig",
	3735: "regconfig[]", 3769: "regdictionary", 3770: "regdictionary[]", 3904: "int4range",
	3905: "int4range[]", 3906: "numrange", 3907: "numrange[]", 3908: "tsrange",
	3909: "tsrange[]", 3910: "tstzrange", 3911: "tstzrange[]", 3912: "daterange",
	3913: "daterange[]", 3926: "int8range", 3927: "int8range[]", 4089: "regnamespace",
	4090: "regnamespace[]", 4096: "regrole", 4097: "regrole[]", 4191: "regcollation",
	4192: "regcollation[]", 4451: "int4multirange", 4532: "nummultirange", 4533: "tsmultirange",
	4534: "tstzmultirange", 4535: "datemultirange", 4536: "int8multirange", 4600: "pg_brin_bloom_summary",
	4601: "pg_brin_minmax_multi_summary", 5039: "pg_snapshot[]", 5069: "xid8", 6150: "int4multirange[]",
	6151: "nummultirange[]", 6152: "tsmultirange[]", 6153: "tstzmultirange[]", 6155: "datemultirange[]",
	6157: "int8multirange[]",
}

// errBinaryOnly is the error for scanning a column of a binary-only type with
// no codec into a string or []byte.
func errBinaryOnly(oid uint32, name string) error {
	return fmt.Errorf("celeris-postgres: scan: column of type %s (OID %d) has no decoder in this driver, so a string or []byte "+
		"would be its raw binary wire form; cast it to text in the query (SELECT col::text) or scan into any", name, oid)
}

// binaryOnlyGuard returns the error for a *string or *[]byte destination of a
// column whose type has no codec and whose binary form is not its text. It is
// nil for every other combination (a codec, text format, another destination, a
// type that is not listed).
func binaryOnlyGuard(dest any, codec *protocol.TypeCodec, col protocol.ColumnDesc, binaryFmt bool) error {
	if codec != nil || !binaryFmt {
		return nil
	}
	switch dest.(type) {
	case *string, *[]byte:
		if name, ok := binaryOnlyTypes[col.TypeOID]; ok {
			return errBinaryOnly(col.TypeOID, name)
		}
	}
	return nil
}
