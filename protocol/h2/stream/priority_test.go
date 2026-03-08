package stream

import "testing"

func TestNewPriorityTree(t *testing.T) {
	pt := NewPriorityTree()
	if pt == nil {
		t.Fatal("NewPriorityTree returned nil")
	}
}

func TestSetGetPriority(t *testing.T) {
	pt := NewPriorityTree()
	pt.SetPriority(1, Priority{
		StreamDependency: 0,
		Weight:           255,
		Exclusive:        false,
	})

	p, ok := pt.GetPriority(1)
	if !ok {
		t.Fatal("GetPriority returned false for existing stream")
	}
	if p.Weight != 255 {
		t.Errorf("Weight: got %d, want 255", p.Weight)
	}
	if p.StreamDependency != 0 {
		t.Errorf("StreamDependency: got %d, want 0", p.StreamDependency)
	}
}

func TestGetWeightDefault(t *testing.T) {
	pt := NewPriorityTree()
	w := pt.GetWeight(1)
	if w != 16 {
		t.Errorf("Default weight: got %d, want 16", w)
	}
}

func TestGetWeightExplicit(t *testing.T) {
	pt := NewPriorityTree()
	pt.SetPriority(1, Priority{Weight: 100})
	w := pt.GetWeight(1)
	if w != 100 {
		t.Errorf("Weight: got %d, want 100", w)
	}
}

func TestExclusiveReparenting(t *testing.T) {
	pt := NewPriorityTree()

	// Stream 3 depends on stream 1
	pt.SetPriority(3, Priority{StreamDependency: 1, Weight: 16})
	// Stream 5 depends on stream 1
	pt.SetPriority(5, Priority{StreamDependency: 1, Weight: 32})

	// Stream 7 becomes exclusive child of stream 1
	// This should make streams 3 and 5 depend on stream 7
	pt.SetPriority(7, Priority{StreamDependency: 1, Weight: 64, Exclusive: true})

	// Check that stream 7 is now the only child of stream 1
	children := pt.GetChildren(1)
	if len(children) != 1 || children[0] != 7 {
		t.Errorf("Children of stream 1: got %v, want [7]", children)
	}

	// Streams 3 and 5 should now depend on stream 7
	p3, ok := pt.GetPriority(3)
	if !ok || p3.StreamDependency != 7 {
		t.Errorf("Stream 3 dependency: got %d, want 7", p3.StreamDependency)
	}
	p5, ok := pt.GetPriority(5)
	if !ok || p5.StreamDependency != 7 {
		t.Errorf("Stream 5 dependency: got %d, want 7", p5.StreamDependency)
	}
}

func TestRemoveStream(t *testing.T) {
	pt := NewPriorityTree()

	// Create dependency chain: 1 -> 3 -> 5
	pt.SetPriority(3, Priority{StreamDependency: 1, Weight: 16})
	pt.SetPriority(5, Priority{StreamDependency: 3, Weight: 32})

	// Remove stream 3; stream 5 should be reparented to stream 1
	pt.RemoveStream(3)

	p5, ok := pt.GetPriority(5)
	if !ok {
		t.Fatal("Stream 5 should still exist")
	}
	if p5.StreamDependency != 1 {
		t.Errorf("Stream 5 dependency after remove: got %d, want 1", p5.StreamDependency)
	}

	_, ok = pt.GetPriority(3)
	if ok {
		t.Error("Stream 3 should be removed")
	}
}

func TestRemoveStreamReorganizesChildren(t *testing.T) {
	pt := NewPriorityTree()

	// Create: 1 -> 3, 3 -> 5, 3 -> 7
	pt.SetPriority(3, Priority{StreamDependency: 1, Weight: 16})
	pt.SetPriority(5, Priority{StreamDependency: 3, Weight: 32})
	pt.SetPriority(7, Priority{StreamDependency: 3, Weight: 48})

	// Remove stream 3; both 5 and 7 should move to stream 1
	pt.RemoveStream(3)

	p5, _ := pt.GetPriority(5)
	if p5.StreamDependency != 1 {
		t.Errorf("Stream 5 dependency: got %d, want 1", p5.StreamDependency)
	}
	p7, _ := pt.GetPriority(7)
	if p7.StreamDependency != 1 {
		t.Errorf("Stream 7 dependency: got %d, want 1", p7.StreamDependency)
	}
}

func TestCalculateStreamPriority(t *testing.T) {
	pt := NewPriorityTree()

	// Root stream with high weight
	pt.SetPriority(1, Priority{StreamDependency: 0, Weight: 255})
	score1 := pt.CalculateStreamPriority(1)

	// Deep stream with low weight
	pt.SetPriority(3, Priority{StreamDependency: 1, Weight: 1})
	pt.SetPriority(5, Priority{StreamDependency: 3, Weight: 1})
	score5 := pt.CalculateStreamPriority(5)

	if score1 <= score5 {
		t.Errorf("Root stream score (%d) should be > deep stream score (%d)", score1, score5)
	}
}

func TestCalculateStreamPriorityUnknown(t *testing.T) {
	pt := NewPriorityTree()
	score := pt.CalculateStreamPriority(99)
	// Default weight is 16 and no depth adjustments
	if score != 16 {
		t.Errorf("Unknown stream priority: got %d, want 16", score)
	}
}

func TestGetChildren(t *testing.T) {
	pt := NewPriorityTree()
	pt.SetPriority(3, Priority{StreamDependency: 1, Weight: 16})
	pt.SetPriority(5, Priority{StreamDependency: 1, Weight: 32})

	children := pt.GetChildren(1)
	if len(children) != 2 {
		t.Errorf("Children of stream 1: got %d, want 2", len(children))
	}

	noChildren := pt.GetChildren(99)
	if noChildren != nil {
		t.Errorf("Children of non-existent stream: got %v, want nil", noChildren)
	}
}

func TestUpdateFromFrame(t *testing.T) {
	pt := NewPriorityTree()
	pt.UpdateFromFrame(1, 0, 128, false)

	p, ok := pt.GetPriority(1)
	if !ok {
		t.Fatal("Stream 1 not found")
	}
	if p.Weight != 128 {
		t.Errorf("Weight: got %d, want 128", p.Weight)
	}
}

func TestUpdateFromFrameSelfDependency(t *testing.T) {
	pt := NewPriorityTree()
	// Self-dependency should be normalized to 0
	pt.UpdateFromFrame(1, 1, 128, false)

	p, ok := pt.GetPriority(1)
	if !ok {
		t.Fatal("Stream 1 not found")
	}
	if p.StreamDependency != 0 {
		t.Errorf("Self-dependency should be normalized to 0, got %d", p.StreamDependency)
	}
}
