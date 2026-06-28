//go:build linux

package adaptive

import (
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
)

// applyTransplant wires the cross-engine drain after a switch — BIDIRECTIONAL
// (#383 + reverse-direction prototype):
//   - promote (newActive == io_uring): the standby epoll drains its idle
//     keep-alives onto io_uring; io_uring stops any reverse drain.
//   - revert  (newActive == epoll): the standby io_uring drains its idle
//     keep-alives back to epoll; epoll stops its forward drain.
//
// Each engine is both a source (StartTransplant → drain TO the target) and a
// destination (AdoptConn). Only the concrete epoll↔io_uring pair supports
// transplant; any other combination is a silent no-op.
func (e *Engine) applyTransplant(newActive, newStandby engine.Engine) {
	switch active := newActive.(type) {
	case *iouring.Engine:
		// Promote epoll→io_uring: drain epoll's conns onto io_uring.
		active.StopTransplant() // halt any reverse (io_uring→epoll) drain
		if ep, ok := newStandby.(*epoll.Engine); ok {
			ep.StartTransplant(active)
			e.logger.Info("transplant: draining epoll keep-alives to io_uring (#383)")
		}
	case *epoll.Engine:
		// Revert io_uring→epoll: drain io_uring's conns back to epoll.
		active.StopTransplant() // halt any forward (epoll→io_uring) drain
		if iou, ok := newStandby.(*iouring.Engine); ok {
			iou.StartTransplant(active)
			e.logger.Info("transplant: draining io_uring keep-alives back to epoll (#383 reverse)")
		}
	}
}
