// Package pgspec is a PostgreSQL v3 wire protocol compliance verification
// suite for the celeris native PostgreSQL driver's protocol package.
//
// It follows the h2spec/Autobahn pattern: each test sends raw wire messages
// over a TCP connection to a live PostgreSQL server, using the celeris
// protocol package (driver/postgres/protocol) to build and parse messages.
// This exercises the PROTOCOL layer directly -- wire framing, message types,
// state transitions -- not application logic or database/sql semantics.
//
// Tests are organized by PostgreSQL protocol specification sections:
//
//   - Section 1: Startup (protocol-flow.html, 55.2.1)
//   - Section 2: Authentication (55.2.2)
//   - Section 3: Simple Query (55.2.3)
//   - Section 4: Extended Query (55.2.4)
//   - Section 5: COPY (55.2.5)
//   - Section 6: Error Handling (55.2.6)
//   - Section 7: Wire Framing Edge Cases
//   - Section 8: Type Round-trips
//   - Section 9: Connection Lifecycle
//
// Build tag:
//
//	//go:build pgspec
//
// Environment variable:
//
//	CELERIS_PG_DSN=postgres://user:pass@host:port/db?sslmode=disable
//
// Example invocation:
//
//	CELERIS_PG_DSN='postgres://postgres:celeris@127.0.0.1:5432/celeristest?sslmode=disable' \
//	  go test -tags pgspec -count=1 -timeout=300s -v ./test/pgspec/...
package pgspec
