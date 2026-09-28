//go:build !validation

package recvtheft

// Enabled is false in production: the engine's call sites are guarded by it
// and compile away.
const Enabled = false

// ArmSeq is zero-size in production: a connection carries no recv sequence.
type ArmSeq struct{}

// Set is the production no-op.
func (*ArmSeq) Set(uint32) {}

// Get is the production no-op.
func (*ArmSeq) Get() uint32 { return 0 }

// NoteCloseWithUnsubmittedRecv is the production no-op.
func NoteCloseWithUnsubmittedRecv() {}

// CloseWithUnsubmittedRecv is always 0 in production.
func CloseWithUnsubmittedRecv() uint64 { return 0 }

// SubmitBeforeClose is always false in production.
func SubmitBeforeClose() bool { return false }

// NoteCloseWithLinkedRecv is the production no-op.
func NoteCloseWithLinkedRecv() {}

// CloseWithLinkedRecv is always 0 in production.
func CloseWithLinkedRecv() uint64 { return 0 }

// HoldAfterClose is the production no-op.
func HoldAfterClose(int, int, bool) {}

// AfterAccept is the production no-op.
func AfterAccept(int, int) {}

// AfterPromoteArm is the production no-op.
func AfterPromoteArm(func() bool) {}

// NoteStaleRecvData is the production no-op.
func NoteStaleRecvData(int, int, uint32, int32, []byte) {}

// WakeHold is the production no-op.
func WakeHold() {}

// NoteHijackWithOpOwed is the production no-op.
func NoteHijackWithOpOwed() {}

// HijackWithOpOwed is always 0 in production.
func HijackWithOpOwed() uint64 { return 0 }

// HijackHold is the production no-op.
func HijackHold() {}
