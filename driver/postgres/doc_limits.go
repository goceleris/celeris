// Package postgres connects exactly as v1.5.11 did, and reads the session settings it
// does not set (see below).
//
// # Session settings the driver does not set
//
// The StartupMessage carries the DSN's own parameters and nothing else, so the
// driver connects through PgBouncer and other poolers exactly as v1.5.11 did (a
// pooler refuses a startup parameter it does not track). The extended protocol
// (every statement with arguments, cached, prepared or unnamed) asks for binary
// results, which do not depend on session settings. The simple protocol (a
// statement without arguments) reads the server's text, which a role or
// database can change. The driver reads what it can:
//
//   - bytea_output: the text decoder reads the hex form and the escape form.
//   - DateStyle: date, timestamp and timestamptz text is read in the ISO styles
//     (the server reports "ISO, MDY", "ISO, DMY" or "ISO, YMD"). A role or
//     database set to the SQL, Postgres or German style makes a text-format
//     date or timestamp an error that names DateStyle (it was a parse error in
//     v1.5.11); it is never read as another value. Set DateStyle to ISO on the
//     role or database, or give the statement an argument.
//
// # Known limit: extra_float_digits on the simple protocol
//
// extra_float_digits cannot be read back: the server does not report it in
// ParameterStatus. Since PostgreSQL 12 the default (1) prints float4 and
// float8 in the shortest form that reads back exactly. A role or database set
// to 0 or less (ALTER ROLE ... SET extra_float_digits = 0) makes the server
// print 15 (float8) or 6 (float4) significant digits, so a float8 such as
// 0.30000000000000004 reads back as 0.3 on the simple protocol, as it did in
// v1.5.11. Statements with arguments use the binary format and are exact.
// Workaround: set extra_float_digits to 1 or more on the role or database, or
// give the statement an argument. Tracked in goceleris/celeris#1364.
//
// # Types without a codec
//
// The driver has no codec for time, timetz, interval, inet, cidr, macaddr,
// macaddr8, oid, money, bit, the geometric types, tsvector, tsquery, ranges, and
// the arrays of types without an array codec. On the extended protocol such a
// column arrives in binary: scanning it into *string or *[]byte is an error that
// names the type and says to cast it to text in the query (SELECT col::text),
// *any and a sql.Scanner receive the raw bytes as in v1.5.11, and on the simple
// protocol the text is the value. A type the driver does not know at all (an
// enum, a composite) is left as it was: an enum's binary form is its text.
// Through database/sql the raw bytes still reach a *string, because database/sql
// converts the driver's []byte itself: cast to text there. Typed codecs are
// tracked in goceleris/celeris#1352.
package postgres
