package conn

import "testing"

// TestH2ShardedQueue_OddStreamShardDistribution is a regression guard for
// celeris#406. HTTP/2 client request streams are always odd (RFC 9113 §5.1.1)
// and Celeris does no server push, so the old streamID%h2QueueShards collapsed
// every response onto the odd residues (shards 1,3) and left 0,2 permanently
// dead — a 4-shard queue that behaved like 2. The (streamID>>1) hash must
// spread odd IDs across ALL shards while keeping each stream affine to one.
func TestH2ShardedQueue_OddStreamShardDistribution(t *testing.T) {
	var q h2ShardedQueue
	q.wakeupFD = -1 // pure in-memory; no eventfd signaling

	// Enqueue one frame per odd stream ID (1,3,5,…), like real client streams.
	const perShard = 4
	n := perShard * h2QueueShards
	for i := 0; i < n; i++ {
		q.Enqueue(uint32(2*i+1), getH2FrameBuf())
	}

	// Every shard must be exercised, evenly. Fails on the old %4 (shards 0,2
	// get 0 bufs).
	for i := range q.shards {
		if got := len(q.shards[i].bufs); got != perShard {
			t.Fatalf("shard %d got %d bufs, want %d: odd stream IDs not spread across all %d shards (celeris#406)",
				i, got, perShard, h2QueueShards)
		}
	}

	// Stream affinity: the same stream ID always lands in exactly one shard.
	var q2 h2ShardedQueue
	q2.wakeupFD = -1
	const affID = 5
	for i := 0; i < 3; i++ {
		q2.Enqueue(affID, getH2FrameBuf())
	}
	nonEmpty := 0
	for i := range q2.shards {
		switch len(q2.shards[i].bufs) {
		case 0:
		case 3:
			nonEmpty++
		default:
			t.Fatalf("stream %d split across shards: shard %d has %d bufs", affID, i, len(q2.shards[i].bufs))
		}
	}
	if nonEmpty != 1 {
		t.Fatalf("stream %d landed in %d shards, want exactly 1", affID, nonEmpty)
	}

	// DrainTo must return every buffer, in order, from the single-writer drain.
	got := 0
	q.DrainTo(func([]byte) { got++ })
	if got != n {
		t.Fatalf("DrainTo wrote %d bufs, want %d", got, n)
	}
}
