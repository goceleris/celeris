// Package recvtheft holds the witness and the hold points of the celeris#715
// recv-theft measurement (hypothesis (a) of that issue, the celeris#685 class).
// Everything here is compiled in only under -tags=validation (recvtheft.go);
// production builds get the no-ops in recvtheft_off.go, whose false Enabled
// constant compiles the engine's call sites away, and whose ArmSeq is a
// zero-size struct. It is internal, like internal/zcwindow, so nothing outside
// this module can call it.
//
// The window. The io_uring worker only PREPARES a recv SQE; it reaches the
// kernel at the next io_uring_enter, at the top of the next loop iteration.
// Fixed files are off, so the SQE names the descriptor NUMBER, and the kernel
// resolves that number when it issues the op, not when the SQE was written.
// finishClose and finishCloseDetached queue an ASYNC_CANCEL behind such an
// unsubmitted recv and then close the descriptor synchronously. If another
// thread's accept is given the freed number before this worker submits, the
// recv reads the new connection's request, completes under the old
// connection's (fd, generation), and is dropped as stale; the new connection
// then waits on an empty socket.
//
// The pieces:
//   - a witness counter, [CloseWithUnsubmittedRecv]: a close reached while
//     the connection's recv SQE had not been consumed by the kernel;
//   - a close hold ([HoldAfterClose]) that parks the closing worker right
//     after the close, and an accept hold ([AfterAccept]) that parks a
//     sibling worker right after it accepted the freed number, before it
//     submits its own recv for it. Together they force the one ordering the
//     theft needs: the sibling's accept after the close, the closing worker's
//     submit before the sibling's;
//   - a gate ([AfterPromoteArm]) that makes the dispatch goroutine's close
//     land in the same loop iteration as the promote's recv re-arm;
//   - the control switch ([Options.SubmitBeforeClose]): submit before the
//     close, so the recv is issued while the number still names the closing
//     connection's socket;
//   - an exemplar record of every stale recv completion that carried data
//     ([NoteStaleRecvData]), with its first bytes;
//   - a separate hold for hypothesis (c) ([WakeHold]): the dispatch
//     goroutine's wake-up delayed between the worker's asyncInMu unlock and
//     its Signal / goroutine start.
package recvtheft
