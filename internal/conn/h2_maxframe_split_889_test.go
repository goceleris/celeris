package conn

import (
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889, review round 1 of PR #1005: a frame of exactly the advertised
// SETTINGS_MAX_FRAME_SIZE that arrives in two reads is waited for, not acted
// on from its header (hasCompleteFrame refuses early only a frame LONGER than
// the maximum). The common upload shape: 16384-byte DATA frames cut by TCP
// segments. A boundary of `>=` closes the connection of an honest client:
// the framer reads half a payload and gets io.ErrUnexpectedEOF.
func TestProcessH2_MaxSizeFrameSplitWaits889(t *testing.T) {
	h := &captureHandler{}
	p := newPeer889(t, h, H2Config{MaxFrameSize: 16384})
	if err := p.send(preface889(t)); err != nil {
		t.Fatal(err)
	}
	// An unknown frame type (0x0b) of exactly 16384 bytes: legal, and ignored once whole.
	whole := rawFrame889(t, http2.FrameType(0x0b), 0, 0, make([]byte, 16384))
	if err := p.send(whole[:len(whole)-100]); err != nil {
		t.Fatalf("the first part of a frame of exactly the maximum size ended the connection: %v", err)
	}
	if err := p.send(whole[len(whole)-100:]); err != nil {
		t.Fatalf("the rest of the frame: %v", err)
	}
	if got := p.goAways(); len(got) != 0 {
		t.Fatalf("GOAWAYs = %+v, want none", got)
	}
}
