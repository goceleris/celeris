//go:build linux

package iouring

import (
	"reflect"
	"sync/atomic"
	"testing"
	"unsafe"
)

// atomicBoolFields returns every atomic.Bool field of connState by name,
// discovered reflectively so a newly added flag is covered automatically.
func atomicBoolFields(t *testing.T, cs *connState) map[string]*atomic.Bool {
	t.Helper()
	out := make(map[string]*atomic.Bool)
	v := reflect.ValueOf(cs).Elem()
	typ := v.Type()
	want := reflect.TypeOf(atomic.Bool{})
	for i := range typ.NumField() {
		if typ.Field(i).Type != want {
			continue
		}
		//nolint:gosec // same-package test reading an unexported field by address
		out[typ.Field(i).Name] = (*atomic.Bool)(unsafe.Pointer(v.Field(i).UnsafeAddr()))
	}
	if len(out) == 0 {
		t.Fatal("no atomic.Bool fields discovered — the reflection walk is broken, not the code")
	}
	return out
}

// TestReleaseConnStateResetsEveryAtomicBool pins celeris#544.
//
// releaseConnState hands the connState back to connStatePool, so any flag it
// forgets is inherited by the next connection to acquire it. asyncH2Promoted
// was missed while all four of its siblings were reset, and a stale true value
// makes asyncTransplantEligible refuse that connection forever — it silently
// never drains to epoll on an adaptive revert.
//
// The fields are enumerated by reflection on purpose. A hand-written list is
// exactly what failed here, and it had already been found incomplete once
// before, so this asserts the property rather than today's field names.
func TestReleaseConnStateResetsEveryAtomicBool(t *testing.T) {
	cs := &connState{}
	fields := atomicBoolFields(t, cs)
	for _, f := range fields {
		f.Store(true)
	}

	releaseConnState(cs)

	for name, f := range fields {
		if f.Load() {
			t.Errorf("releaseConnState left %s set; the next connection out of "+
				"connStatePool inherits it", name)
		}
	}
}

// TestConnStateHasNoWritevFields pins celeris#884 item 31: io_uring has no
// scatter-gather send. cs.bodyBuf and cs.sendBody were never assigned a
// non-nil slice once the zero-copy body writer stopped being installed
// (celeris#817), so the WRITEV branches of flushSend, flushSendLink and
// completeSend, and the connState fields they read, were dead code that every
// "is a send owed" test still had to carry a term for. A field of the old
// names, or of the iovec type, would bring the path back without its writer:
// a body the kernel reads only at the next io_uring_enter, after the handler
// that owns it may have reused it.
func TestConnStateHasNoWritevFields(t *testing.T) {
	typ := reflect.TypeOf(connState{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		switch f.Name {
		case "bodyBuf", "sendBody", "iov":
			t.Errorf("connState.%s is back: the WRITEV path has no writer (celeris#817), see celeris#884 item 31", f.Name)
		}
		ft := f.Type
		for ft.Kind() == reflect.Array || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Name() == "iovec" {
			t.Errorf("connState.%s has type iovec: the WRITEV path has no writer (celeris#817), see celeris#884 item 31", f.Name)
		}
	}
}
