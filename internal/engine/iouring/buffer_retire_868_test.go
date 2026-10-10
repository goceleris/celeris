//go:build linux

package iouring

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mapped reports whether the page holding addr is mapped (mincore answers
// ENOMEM for an address that is not).
func mapped(t *testing.T, p unsafe.Pointer) bool {
	t.Helper()
	page := uintptr(os.Getpagesize())
	base := uintptr(p) &^ (page - 1)
	vec := make([]byte, 1)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, base, page, uintptr(unsafe.Pointer(&vec[0])))
	var err error
	if errno != 0 {
		err = errno
	}
	switch err {
	case nil:
		return true
	case unix.ENOMEM:
		return false
	}
	t.Fatalf("mincore: %v", err)
	return false
}

// TestBufferRingRetireKeepsBufferAndRefreshesRing pins celeris#868 at the
// ring: RetireBuffer leaves the retired buffer's bytes alone, however often
// the ring is written afterwards, and puts a fresh entry for the same buffer
// ID in the ring, pointing elsewhere.
func TestBufferRingRetireKeepsBufferAndRefreshesRing(t *testing.T) {
	ring := newTestRing(t)
	br, err := NewBufferRing(ring, 7, 8, 1024)
	if err != nil {
		t.Fatalf("NewBufferRing: %v", err)
	}
	defer br.Close(ring)

	const id = 3
	old := br.GetBuffer(id, 16)
	copy(old, "hijacker-request")
	oldAddr := uintptr(unsafe.Pointer(&old[0]))
	tail := br.tail

	br.RetireBuffer(id)

	if br.tail != tail+1 {
		t.Fatalf("RetireBuffer pushed %d ring entries, want 1", br.tail-tail)
	}
	entry := (*bufRingEntry)(unsafe.Add(br.ringAddr, uintptr((br.tail-1)&br.mask)*bufRingEntrySize))
	if entry.Bid != id {
		t.Fatalf("fresh entry has buffer ID %d, want %d", entry.Bid, id)
	}
	if uintptr(entry.Addr) == oldAddr {
		t.Fatalf("fresh entry points at the retired buffer (%#x)", oldAddr)
	}
	fresh := br.GetBuffer(id, 16)
	if uintptr(unsafe.Pointer(&fresh[0])) != uintptr(entry.Addr) {
		t.Fatalf("GetBuffer(%d) = %p, the ring's entry says %#x", id, &fresh[0], entry.Addr)
	}
	// The kernel receives the next request into the fresh buffer.
	copy(fresh, "another-conn-req")
	if string(old) != "hijacker-request" {
		t.Fatalf("retired buffer changed: %q", old)
	}

	// Pushed back the ordinary way, the fresh buffer is the entry's address.
	br.PushBuffer(id)
	entry = (*bufRingEntry)(unsafe.Add(br.ringAddr, uintptr((br.tail-1)&br.mask)*bufRingEntrySize))
	if uintptr(entry.Addr) != uintptr(unsafe.Pointer(&fresh[0])) {
		t.Fatalf("PushBuffer after a retirement re-offered %#x, want the fresh buffer %p", entry.Addr, &fresh[0])
	}

	// Retiring the same slot again retires the fresh buffer; the slot's mmap
	// bytes are kept once, however often.
	br.RetireBuffer(id)
	if got := br.Retired(); got != 2 {
		t.Fatalf("Retired() = %d, want 2", got)
	}
	if len(br.kept) != 1 {
		t.Fatalf("kept = %v, want slot %d once", br.kept, id)
	}
	if string(fresh) != "another-conn-req" {
		t.Fatalf("second retirement changed the first fresh buffer: %q", fresh)
	}
}

// TestBufferRingCloseKeepsOnlyRetiredPages pins the other half of
// celeris#868's fix: Close must not unmap what a hijacker's strings may still
// view, and must unmap the rest, so the memory kept is the retired slots'
// pages and no more.
func TestBufferRingCloseKeepsOnlyRetiredPages(t *testing.T) {
	page := os.Getpagesize()
	for _, tc := range []struct {
		name    string
		size    int
		retire  []uint16
		keep    []uint16 // slots whose page must stay mapped
		release []uint16 // slots whose page must be unmapped
	}{
		// Page-sized slots: only the retired slots' own pages stay.
		{"page-sized", page, []uint16{2, 9}, []uint16{2, 9}, []uint16{0, 1, 3, 8, 10, 15}},
		// Sub-page slots share pages: the whole page of a retired slot stays.
		{"sub-page", page / 4, []uint16{5}, []uint16{4, 5, 6, 7}, []uint16{0, 3, 8, 15}},
		// Slots at both ends of the region.
		{"edges", page, []uint16{0, 15}, []uint16{0, 15}, []uint16{1, 7, 14}},
		// Nothing retired: nothing kept.
		{"none", page, nil, nil, []uint16{0, 7, 15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ring := newTestRing(t)
			br, err := NewBufferRing(ring, 7, 16, tc.size)
			if err != nil {
				t.Fatalf("NewBufferRing: %v", err)
			}
			addr := func(id uint16) unsafe.Pointer {
				return unsafe.Pointer(uintptr(unsafe.Pointer(&br.bufRegion[0])) + uintptr(id)*uintptr(tc.size))
			}
			keepAddr := map[uint16]unsafe.Pointer{}
			releaseAddr := map[uint16]unsafe.Pointer{}
			for _, id := range tc.keep {
				keepAddr[id] = addr(id)
			}
			for _, id := range tc.release {
				releaseAddr[id] = addr(id)
			}
			var views [][]byte
			for _, id := range tc.retire {
				v := br.GetBuffer(id, 8)
				copy(v, "retired!")
				views = append(views, v)
				br.RetireBuffer(id)
			}
			br.Close(ring)
			for id, p := range keepAddr {
				if !mapped(t, p) {
					t.Errorf("slot %d (size %d) unmapped by Close, but a retired buffer still is its bytes", id, tc.size)
				}
			}
			for id, p := range releaseAddr {
				if mapped(t, p) {
					t.Errorf("slot %d (size %d) still mapped after Close: the memory kept is more than the retired slots' pages", id, tc.size)
				}
			}
			for i, v := range views {
				if string(v) != "retired!" {
					t.Errorf("retired view %d reads %q after Close", i, v)
				}
			}
		})
	}
}

// TestBufferRingRetireCost states what celeris#868's fix costs, in
// allocations (deterministic, unlike a timing): the ordinary return of a
// buffer stays allocation-free, as does reading one, and a retirement is one
// allocation of one buffer.
func TestBufferRingRetireCost(t *testing.T) {
	ring := newTestRing(t)
	const size = 8192
	br, err := NewBufferRing(ring, 7, 64, size)
	if err != nil {
		t.Fatalf("NewBufferRing: %v", err)
	}
	defer br.Close(ring)

	push := testing.AllocsPerRun(100, func() { br.PushBuffer(1) })
	get := testing.AllocsPerRun(100, func() { _ = br.GetBuffer(1, 100) })
	if push != 0 || get != 0 {
		t.Errorf("before any retirement: PushBuffer %v allocs, GetBuffer %v allocs, want 0 and 0", push, get)
	}
	retire := testing.AllocsPerRun(100, func() { br.RetireBuffer(2) })
	if retire != 1 {
		t.Errorf("RetireBuffer: %v allocs, want 1 (one fresh buffer)", retire)
	}
	// With a retirement on record the ordinary paths stay allocation-free.
	push = testing.AllocsPerRun(100, func() { br.PushBuffer(1) })
	get = testing.AllocsPerRun(100, func() { _ = br.GetBuffer(2, 100) })
	if push != 0 || get != 0 {
		t.Errorf("after a retirement: PushBuffer %v allocs, GetBuffer %v allocs, want 0 and 0", push, get)
	}
	t.Logf("C868COST push_allocs=%v get_allocs=%v retire_allocs=%v retire_bytes=%d", push, get, retire, size)
}
