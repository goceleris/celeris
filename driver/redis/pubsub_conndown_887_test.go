package redis

// celeris#887 item 11: the control calls of a PubSub whose conn is down return
// an exported sentinel, so callers can tell "not sent, the reconnect replays
// it" from a closed PubSub and from a real write failure. The calls that return
// it are exercised by pubsub_ctlwrite_929_linux_test.go; this pins what the
// exported value promises.

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrPubSubConnDownIsMatchableAndDistinct887(t *testing.T) {
	wrapped := fmt.Errorf("subscribe orders: %w", ErrPubSubConnDown)
	if !errors.Is(wrapped, ErrPubSubConnDown) {
		t.Error("errors.Is does not match a wrapped ErrPubSubConnDown")
	}
	for name, other := range map[string]error{"ErrClosed": ErrClosed, "ErrUnknownFD": ErrUnknownFD, "ErrQueueFull": ErrQueueFull} {
		if errors.Is(ErrPubSubConnDown, other) || errors.Is(other, ErrPubSubConnDown) {
			t.Errorf("ErrPubSubConnDown and %s match each other: callers could not tell them apart", name)
		}
	}
	if ErrPubSubConnDown.Error() == "" {
		t.Error("ErrPubSubConnDown has no message")
	}
}
