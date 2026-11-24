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
// # Types decoded to their text
//
// The binary result of time, timetz, interval, inet, cidr, macaddr, macaddr8 and
// oid is decoded to the text the server prints for the value: a string (an int64
// for oid) into *any, and the same text into *string, *[]byte and, through
// database/sql, *string. The text is the server's ::text for the value under the
// default settings (inet: its output function, which leaves /32 and /128 off, not
// the text(inet) cast, which keeps them). An interval is always in the
// IntervalStyle postgres form (1 year 2 mons -3 days +04:05:06.789), whatever the
// session's IntervalStyle, because the binary form carries no style; the simple
// protocol returns the server's text in the session's style. An interval, time
// or timetz is not decoded to a Go time or duration: scan it into a string, or
// cast it in the query (EXTRACT(EPOCH FROM ...)). Arrays of these types have no
// codec (below).
//
// # Types without a codec
//
// The driver has no codec for money, bit, the geometric types, tsvector, tsquery,
// ranges, and the arrays of types without an array codec (numeric[], date[],
// timestamp[], time[], interval[], inet[], oid[] ...). On the extended protocol
// such a column arrives in binary: scanning it into *string or *[]byte is an
// error that names the type and says to cast it to text in the query (SELECT
// col::text), *any and a sql.Scanner receive the raw bytes as in v1.5.11, and on
// the simple protocol the text is the value. A type the driver does not know at
// all (an enum, a composite) is left as it was: an enum's binary form is its
// text. Through database/sql the raw bytes still reach a *string, because
// database/sql converts the driver's []byte itself: cast to text there. Typed
// codecs are tracked in goceleris/celeris#1352.
package postgres
