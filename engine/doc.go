// Package engine defines the core [Engine] interface and associated types used
// by all I/O engine implementations (io_uring, epoll, adaptive, std).
//
// This package is the dependency root for engine-related types. User code
// typically interacts with engines through the top-level celeris package
// rather than importing this package directly.
package engine
