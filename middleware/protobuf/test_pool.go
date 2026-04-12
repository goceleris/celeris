package protobuf

import (
	"testing"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"google.golang.org/protobuf/proto"
)

// Test that demonstrates the pool behavior
func TestPoolBehavior(t *testing.T) {
	// Get a buffer from the pool
	bp1 := bufPool.Get().(*[]byte)
	cap1 := cap(*bp1)
	ptr1 := &(*bp1)[0]
	
	// Marshal something small
	msg1 := wrapperspb.String("small")
	data1, _ := proto.MarshalOptions{}.MarshalAppend(*bp1, msg1)
	
	// Check if data1 reused the same backing array
	if len(data1) > 0 {
		ptr1_after := &data1[0]
		t.Logf("Before: cap=%d, ptr=%p", cap1, ptr1)
		t.Logf("After small: len=%d, cap=%d, ptr=%p, same=%v", len(data1), cap(data1), ptr1_after, ptr1 == ptr1_after)
	}
	
	// Put it back
	*bp1 = data1
	bufPool.Put(bp1)
	
	// Get it again
	bp2 := bufPool.Get().(*[]byte)
	if bp2 != bp1 {
		t.Fatal("Pool returned different pointer!")
	}
	
	// The contents should be from the previous marshal
	t.Logf("After re-get: len=%d, cap=%d", len(*bp2), cap(*bp2))
	
	// Marshal something large
	msg2 := wrapperspb.Bytes(make([]byte, 40*1024))
	data2, _ := proto.MarshalOptions{}.MarshalAppend(*bp2, msg2)
	
	t.Logf("After large: len=%d, cap=%d, exceeds limit=%v", len(data2), cap(data2), cap(data2) > 32*1024)
}
