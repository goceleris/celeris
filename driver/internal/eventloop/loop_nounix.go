//go:build !unix

package eventloop

// newLoop on a platform without unix file descriptors (windows): the portable
// workers, which adopt a net.Conn (see [NetConnRegistrar]) because a
// descriptor number cannot be turned into one here.
func newLoop(workers int) (*Loop, error) { return NewPortable(workers) }
