// Package celeris registers every benched celeris configuration
// (Engine × Protocol × Upgrade × Async → 35 distinct cell-columns) as a
// [servers.Server] against the perfmatrix registry.
//
// Wave-2 populates the init() block with concrete [servers.Server]
// implementations; the scaffold defines the naming convention the
// registered servers MUST follow:
//
//	celeris-<engine>-<protocol>[-upgrade][+async]
//
// Examples:
//
//	celeris-iouring-h1
//	celeris-iouring-auto
//	celeris-iouring-auto-upgrade
//	celeris-iouring-auto-upgrade+async
//	celeris-epoll-h2c
//	celeris-std-auto
package celeris

// Engines enumerates the celeris engines sampled by the matrix. Order
// is stable so cell-column IDs are reproducible.
var Engines = []string{"iouring", "epoll", "adaptive", "std"}

// Protocols enumerates the protocols sampled by the matrix.
var Protocols = []string{"h1", "h2c", "auto"}

// Wave-2 fills in init() with servers.Register calls for each
// (engine, protocol, upgrade?, async?) combination.
func init() {}
