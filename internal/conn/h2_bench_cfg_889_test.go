package conn

import "time"

// benchCfg889 is the H2Config the request-deadline benchmarks run with: the
// defaults of ReadTimeout, ReadHeaderTimeout and WriteTimeout, so that a request
// with a body arms its deadline and the write deadline is on.
func benchCfg889() H2Config {
	return H2Config{ReadTimeout: 60 * time.Second, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
}
