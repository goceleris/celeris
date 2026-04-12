package protobuf

import (
	"testing"
	"unsafe"
)

// Test to verify the pool safety issue with stack-allocated pointers
func TestPoolStackAllocation(t *testing.T) {
	// The New() function allocates a slice on the stack: b := make([]byte, 0, 256)
	// Then returns &b
	
	// Get a value from the pool
	bp := bufPool.Get().(*[]byte)
	addr1 := uintptr(unsafe.Pointer(bp))
	
	t.Logf("First pool.Get(): pointer addr=%v", addr1)
	
	// Put it back
	bufPool.Put(bp)
	
	// Get another value
	bp2 := bufPool.Get().(*[]byte)
	addr2 := uintptr(unsafe.Pointer(bp2))
	
	t.Logf("Second pool.Get(): pointer addr=%v", addr2)
	
	if bp == bp2 {
		t.Logf("Pool returned same pointer (expected reuse)")
	}
	
	// The issue: if bp is a pointer to a stack variable from New(),
	// its address should be invalid. But it seems to work...
	
	// Try to use it
	*bp2 = make([]byte, 10, 20)
	t.Logf("Successfully assigned to dereferenced pointer")
}
