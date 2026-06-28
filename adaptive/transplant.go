//go:build linux

package adaptive

import (
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
)

// applyTransplant wires (or unwinds) the cross-engine drain after a switch.
//   - promote (newActive == io_uring): tell the standby epoll to start handing
//     its idle keep-alive conns to io_uring at their next request boundary.
//   - revert  (newActive == epoll): stop any in-progress drain so freshly
//     accepted conns are not migrated away from the now-active epoll.
//
// Only the concrete epoll↔io_uring pair supports transplant; any other
// combination is a silent no-op.
func (e *Engine) applyTransplant(newActive, newStandby engine.Engine) {
	switch active := newActive.(type) {
	case *iouring.Engine:
		if ep, ok := newStandby.(*epoll.Engine); ok {
			ep.StartTransplant(active)
			e.logger.Info("transplant: draining epoll keep-alives to io_uring (#383)")
		}
	case *epoll.Engine:
		active.StopTransplant()
		e.logger.Info("transplant: drain stopped, epoll re-active (#383)")
	}
}
