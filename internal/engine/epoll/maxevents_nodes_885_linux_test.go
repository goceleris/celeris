//go:build linux

package epoll

import (
	"testing"

	"github.com/goceleris/celeris/internal/platform"
)

// celeris#885 item 31. Listen divided MaxEvents by the host's NUMA node count,
// not by the nodes the process may use: a process kept to one node (numactl
// --cpunodebind, a single-numa-node pod, a cpuset of one socket) has every loop
// on that node and still got MaxEvents / NumNodes. The split follows the nodes
// the plan's allowed CPU set meets.

// nodes885 is a host of n nodes with one CPU each (node i holds CPU i).
func nodes885(n int) platform.NUMATopology {
	t := platform.NUMATopology{NumNodes: n}
	for i := range n {
		t.NodeCPUs = append(t.NodeCPUs, []int{i})
	}
	return t
}

// seq885 is the CPUs 0..n-1.
func seq885(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func TestSplitMaxEventsFollowsTheNodesTheAllowedSetMeets885(t *testing.T) {
	for _, tc := range []struct {
		name    string
		topo    platform.NUMATopology
		allowed []int
		want    int
	}{
		{"one-node-host", platform.NUMATopology{NumNodes: 1}, []int{0, 1}, 8192},
		{"restricted-to-one-of-eight", nodes885(8), []int{3}, 8192},
		{"restricted-to-two-of-eight", nodes885(8), []int{2, 5, 5}, 4096},
		{"all-eight", nodes885(8), seq885(8), 1024},
		{"unrestricted-no-plan-set", nodes885(8), nil, 1024}, // no allowed set to read: the host's count, as before
		{"topology-without-cpu-lists", platform.NUMATopology{NumNodes: 4}, []int{0}, 2048},
		{"allowed-meets-no-listed-node", nodes885(4), []int{40}, 2048}, // unknown CPUs: the host's count
		{"floor", nodes885(256), seq885(256), 64},
	} {
		if got := splitMaxEvents(8192, tc.topo, tc.allowed); got != tc.want {
			t.Errorf("%s: splitMaxEvents(8192, %d nodes, allowed %v) = %d, want %d", tc.name, tc.topo.NumNodes, tc.allowed, got, tc.want)
		}
	}
}

// TestListenSplitsMaxEventsByTheNodesOfThePlan885 goes through Listen: a loop's
// event array is min(MaxEvents, 2048), so the host has to be a big one (8
// nodes: 8192/8 = 1024) for the split to show in len(events).
func TestListenSplitsMaxEventsByTheNodesOfThePlan885(t *testing.T) {
	oldN, oldP := detectNUMA, planWorkerCPUs
	t.Cleanup(func() { detectNUMA, planWorkerCPUs = oldN, oldP })
	detectNUMA = func() platform.NUMATopology { return nodes885(8) }
	for _, tc := range []struct {
		name    string
		allowed []int
		want    int
	}{
		{"process-kept-to-one-node", []int{0}, 2048},
		{"process-spans-all-nodes", seq885(8), 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planWorkerCPUs = func(n int, _ bool, _ string) platform.CPUPlan {
				cpus := make([]int, n)
				return platform.CPUPlan{CPUs: cpus, Requested: n, Allowed: tc.allowed}
			}
			r := start895(t, "127.0.0.1:0", 2, false)
			r.e.mu.Lock()
			defer r.e.mu.Unlock()
			if len(r.e.loops) == 0 {
				t.Fatal("no loops")
			}
			for i, l := range r.e.loops {
				if got := len(l.events); got != tc.want {
					t.Errorf("loop %d: len(events) = %d, want %d", i, got, tc.want)
				}
			}
		})
	}
}
