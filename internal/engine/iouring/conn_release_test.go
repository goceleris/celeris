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
