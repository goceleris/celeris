package postgres

import (
	"os"
	"strings"
)

// CELERIS_TEST_STREAMING=1 runs the database/sql carriers of the tests that open
// their *sql.DB with armDSN on the event loop (Connector.WithStreaming, through the
// streaming=true DSN parameter) instead of on the direct connections a Connector
// dials by default since v1.6.0. The Driver Conformance job of ci.yml runs the
// package once more with it, so the loop path of database/sql stays under the
// tests that guard it (the fixes of #1440, #1449, #1459 were made there).
func streamingArm() bool { return os.Getenv("CELERIS_TEST_STREAMING") == "1" }

// armDSN is dsn, with streaming=true when the streaming arm runs.
func armDSN(dsn string) string {
	if !streamingArm() {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "streaming=true"
}
