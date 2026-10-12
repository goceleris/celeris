package postgres

import "testing"

// BenchmarkPgRequestAcquireRelease887 is the per-query cost of the pool round
// trip releasePgRequest sits in: acquire, complete, release. It carries the
// one load the failPins check added to every release (celeris#887 item 20), so
// an A/B of this benchmark between the tree before and after the fix bounds
// that cost. There is no BenchmarkPg* in this package.
func BenchmarkPgRequestAcquireRelease887(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := acquirePgRequest()
		r.finish()
		releasePgRequest(r)
	}
}
