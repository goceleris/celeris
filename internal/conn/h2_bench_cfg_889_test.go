package conn

import "time"

// benchCfg889 is the H2Config the request-deadline benchmarks run with: the
// defaults of ReadTimeout and ReadHeaderTimeout, so that a request with a body
// arms its deadline. (main has no such fields; the comparison builds main with a
// copy of this file that returns H2Config{}.)
func benchCfg889() H2Config {
	return H2Config{ReadTimeout: 60 * time.Second, ReadHeaderTimeout: 10 * time.Second}
}
