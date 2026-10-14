//go:build race

package celeris_test

// raceOn761 reports a -race build. The large-response tests (celeris#761,
// celeris#817) move a few GiB over loopback; under the race detector on CI's
// runners that costs several times what it does elsewhere, so they move less
// there (lean761).
const raceOn761 = true
