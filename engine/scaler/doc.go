// Package scaler implements the dynamic worker scaler shared by the
// iouring, epoll, and adaptive engines. The scaler tracks the active
// connection count and adjusts how many workers participate in the
// SO_REUSEPORT group so connections-per-active-worker stays near the
// configured target — this keeps CQE/event batching density in the
// sweet spot.
//
// Engines plug in via the [Source] interface. Per-engine sources
// (engine/iouring/scaler.go, engine/epoll/scaler.go, adaptive/scaler.go)
// adapt the engine to this contract; the algorithm in [Run] is shared.
//
// Implementation is Linux-only because the underlying engines that
// surface a [Source] are themselves Linux-only. This package compiles
// to an empty stub on non-Linux GOOS so godoc and reflection-based
// tooling still see the public types and contract.
package scaler
