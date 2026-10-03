package conn

import (
	"fmt"
	"testing"
)

// streamInShard837 returns the first odd (client) stream ID that Enqueue maps
// to shard k.
func streamInShard837(k int) uint32 {
	for id := uint32(1); ; id += 2 {
		if int((id>>1)%h2QueueShards) == k {
			return id
		}
	}
}

func frame837(tag string) *[]byte {
	p := getH2FrameBuf()
	*p = append((*p)[:0], tag...)
	return p
}

// TestH2ShardedQueueEnqueueDuringDrainIsNotLost837 is celeris#837. A handler
// goroutine enqueues a frame while the event loop is inside DrainTo, into a
// shard DrainTo has already emptied. The enqueue sees pending still true, so it
// does not signal the loop, and DrainTo used to clear pending at its end: the
// frame then sat in its shard with pending false and no wakeup, and every later
// drain is gated on pending (DrainWriteQueue, ProcessH2's frame loop). A
// streamed response lost its last frames that way until another stream
// enqueued on the connection.
//
// The drain's write callback is the race window, made deterministic: it
// enqueues frames while DrainTo is between shards. After DrainTo returns, a
// drain gated on pending, as the engines run it, must deliver every frame
// enqueued during the first one.
func TestH2ShardedQueueEnqueueDuringDrainIsNotLost837(t *testing.T) {
	for k := 0; k < h2QueueShards; k++ {
		// During the write of shard k's frame, enqueue one frame into every
		// shard: shards 0..k are already emptied (the lost ones on main),
		// shards k+1.. are drained later in the same pass.
		t.Run(fmt.Sprintf("during-shard-%d", k), func(t *testing.T) {
			var q h2ShardedQueue // nil wake handle: Signal is a no-op
			q.Enqueue(streamInShard837(k), frame837("first"))
			if !q.pending.Load() {
				t.Fatal("pending is false after an Enqueue")
			}

			injected := 0
			var firstPass []string
			q.DrainTo(func(b []byte) {
				firstPass = append(firstPass, string(b))
				if string(b) != "first" {
					return
				}
				for j := 0; j < h2QueueShards; j++ {
					q.Enqueue(streamInShard837(j), frame837(fmt.Sprintf("late-%d", j)))
					injected++
				}
			})
			// The injection really ran (a control that never fires would let
			// this test pass on a broken queue).
			if injected != h2QueueShards {
				t.Fatalf("enqueued %d frames during the drain, want %d", injected, h2QueueShards)
			}

			// The engines' next drain: DrainWriteQueue and ProcessH2 drain only
			// while pending is set.
			var secondPass []string
			if q.pending.Load() {
				q.DrainTo(func(b []byte) { secondPass = append(secondPass, string(b)) })
			}

			got := make(map[string]int)
			for _, f := range append(firstPass, secondPass...) {
				got[f]++
			}
			for j := 0; j < h2QueueShards; j++ {
				tag := fmt.Sprintf("late-%d", j)
				if got[tag] != 1 {
					t.Errorf("frame %q (shard %d), enqueued while DrainTo was at shard %d, was delivered %d times, want 1 "+
						"(first pass %q, second pass %q, pending after the first pass gated the second)",
						tag, j, k, got[tag], firstPass, secondPass)
				}
			}
			if got["first"] != 1 {
				t.Errorf("the first frame was delivered %d times, want 1", got["first"])
			}
			for i := range q.shards {
				if n := len(q.shards[i].bufs); n != 0 {
					t.Errorf("shard %d still holds %d frames after the gated drain", i, n)
				}
			}
		})
	}
}
