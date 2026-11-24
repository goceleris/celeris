// Package postgres connects exactly as before, and reads the session settings it
// does not set (see below).
//
// # Session settings the driver does not set
//
// The StartupMessage carries the DSN's
// own parameters and nothing else, so it works behind PgBouncer and other
// poolers, which refuse a startup parameter they do not track. A role or
// database can still change how PostgreSQL prints a result in the text format,
// which the simple protocol and the unnamed extended statement
// (auto_cache_statements=false) use. The driver reads what it can:
//
//   - bytea_output: the text decoder reads the hex form and the escape form.
//   - DateStyle: date, timestamp and timestamptz text is read in the ISO styles
//     (the server reports "ISO, MDY", "ISO, DMY" or "ISO, YMD"). A role or
//     database set to the SQL, Postgres or German style makes a text-format
//     date or timestamp an error that names DateStyle; it is never read as
//     another value. Set DateStyle to ISO on the role or database. Statements
//     that use the binary format (a cached statement, the database/sql
//     prepared path) do not depend on it.
//
// # Known limit: extra_float_digits
//
// extra_float_digits cannot be read back: the server does not report it in
// ParameterStatus. Since PostgreSQL 12 the default (1) prints float4 and
// float8 in the shortest form that reads back exactly. A role or database set
// to 0 or less (ALTER ROLE ... SET extra_float_digits = 0) makes the server
// print 15 (float8) or 6 (float4) significant digits, so a float8 such as
// 0.30000000000000004 reads back as 0.3 on a text-format path: the simple
// protocol and the unnamed extended statement. Cached and prepared statements
// use the binary format and are exact. Workaround: set extra_float_digits to 1
// or more on the role or database, or add it to the DSN as a parameter when the
// server or pooler accepts it. Tracked in goceleris/celeris#1364.
package postgres
