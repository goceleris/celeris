package stream

import (
	"sync"

	"golang.org/x/net/http2"
)

// Priority defines stream dependency and weight for HTTP/2 prioritization.
type Priority struct {
	StreamDependency uint32
	Weight           uint8
	Exclusive        bool
}

// PriorityTree manages stream priorities and dependencies.
type PriorityTree struct {
	mu           sync.RWMutex
	priorities   map[uint32]*Priority
	dependencies map[uint32][]uint32
}

// NewPriorityTree creates a new priority tree.
func NewPriorityTree() *PriorityTree {
	return &PriorityTree{
		priorities:   make(map[uint32]*Priority),
		dependencies: make(map[uint32][]uint32),
	}
}

// SetPriority assigns or updates priority information for a stream.
func (pt *PriorityTree) SetPriority(streamID uint32, priority Priority) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if oldPriority, ok := pt.priorities[streamID]; ok {
		pt.removeDependency(streamID, oldPriority.StreamDependency)
	}

	handled := false
	if priority.Exclusive && priority.StreamDependency != 0 {
		if children, ok := pt.dependencies[priority.StreamDependency]; ok {
			for _, childID := range children {
				if childPriority, exists := pt.priorities[childID]; exists {
					childPriority.StreamDependency = streamID
				}
			}
			pt.dependencies[streamID] = children
			pt.dependencies[priority.StreamDependency] = []uint32{streamID}
			handled = true
		}
	}

	pt.priorities[streamID] = &priority

	if priority.StreamDependency != 0 && !handled {
		pt.dependencies[priority.StreamDependency] = append(
			pt.dependencies[priority.StreamDependency],
			streamID,
		)
	}
}

// GetPriority retrieves priority information for a stream.
func (pt *PriorityTree) GetPriority(streamID uint32) (*Priority, bool) {
	pt.mu.RLock()
	defer pt.mu.RUnlock()
	priority, ok := pt.priorities[streamID]
	return priority, ok
}

// GetWeight returns the weight of a stream, defaulting to 16.
func (pt *PriorityTree) GetWeight(streamID uint32) uint8 {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	if priority, ok := pt.priorities[streamID]; ok {
		return priority.Weight
	}
	return 16
}

// RemoveStream removes a stream and reorganizes its dependencies.
func (pt *PriorityTree) RemoveStream(streamID uint32) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if priority, ok := pt.priorities[streamID]; ok {
		pt.removeDependency(streamID, priority.StreamDependency)

		if children, ok := pt.dependencies[streamID]; ok {
			for _, childID := range children {
				if childPriority, exists := pt.priorities[childID]; exists {
					childPriority.StreamDependency = priority.StreamDependency
					if priority.StreamDependency != 0 {
						pt.dependencies[priority.StreamDependency] = append(
							pt.dependencies[priority.StreamDependency],
							childID,
						)
					}
				}
			}
		}

		delete(pt.priorities, streamID)
		delete(pt.dependencies, streamID)
	}
}

func (pt *PriorityTree) removeDependency(streamID, parentID uint32) {
	if children, ok := pt.dependencies[parentID]; ok {
		for i, childID := range children {
			if childID == streamID {
				pt.dependencies[parentID] = append(children[:i], children[i+1:]...)
				break
			}
		}
	}
}

// CalculateStreamPriority computes a priority score for stream scheduling.
func (pt *PriorityTree) CalculateStreamPriority(streamID uint32) int {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	score := 0
	weight := pt.getWeightLocked(streamID)

	score += int(weight)

	if _, ok := pt.priorities[streamID]; ok {
		depth := 0
		currentID := streamID
		visited := make(map[uint32]bool)

		for depth < 10 {
			if visited[currentID] {
				break
			}
			visited[currentID] = true

			if p, ok := pt.priorities[currentID]; ok {
				if p.StreamDependency == 0 {
					break
				}
				currentID = p.StreamDependency
				depth++
			} else {
				break
			}
		}

		score += (10 - depth) * 10
	}

	return score
}

func (pt *PriorityTree) getWeightLocked(streamID uint32) uint8 {
	if priority, ok := pt.priorities[streamID]; ok {
		return priority.Weight
	}
	return 16
}

// GetChildren returns the streams that depend on the given stream.
func (pt *PriorityTree) GetChildren(streamID uint32) []uint32 {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	if children, ok := pt.dependencies[streamID]; ok {
		result := make([]uint32, len(children))
		copy(result, children)
		return result
	}
	return nil
}

// UpdateFromFrame updates stream priority from frame parameters.
func (pt *PriorityTree) UpdateFromFrame(streamID uint32, dependency uint32, weight uint8, exclusive bool) {
	if streamID == dependency {
		dependency = 0
	}

	pt.SetPriority(streamID, Priority{
		StreamDependency: dependency,
		Weight:           weight,
		Exclusive:        exclusive,
	})
}

// ParsePriorityFromHeaders extracts priority information from a HEADERS frame.
func ParsePriorityFromHeaders(f *http2.HeadersFrame) (dependency uint32, weight uint8, exclusive bool, hasPriority bool) {
	if !f.HasPriority() {
		return 0, 16, false, false
	}

	priority := f.Priority
	return priority.StreamDep, priority.Weight, priority.Exclusive, true
}
