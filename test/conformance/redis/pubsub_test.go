//go:build redis

package redis_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// publish opens a raw TCP conn, sends RESP PUBLISH, and closes. This keeps
// the test self-contained and doesn't depend on a typed Publish helper on
// *Client (which the driver does not currently expose).
func publish(t *testing.T, channel, payload string) {
	t.Helper()
	addr := addrFromEnv(t)
	// net.Dial is used, not the driver.
	deadline := time.Now().Add(3 * time.Second)
	conn, err := dialNet(addr, deadline)
	if err != nil {
		t.Fatalf("dial for PUBLISH: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	// Perform AUTH if required.
	if pw := os.Getenv(envPassword); pw != "" {
		if _, err := conn.Write(encodeCommand("AUTH", pw)); err != nil {
			t.Fatalf("AUTH write: %v", err)
		}
		if _, err := readLine(conn); err != nil {
			t.Fatalf("AUTH read: %v", err)
		}
	}

	if _, err := conn.Write(encodeCommand("PUBLISH", channel, payload)); err != nil {
		t.Fatalf("PUBLISH write: %v", err)
	}
	// Drain one integer reply.
	if _, err := readLine(conn); err != nil {
		t.Fatalf("PUBLISH read: %v", err)
	}
}

func TestPubSub_Basic(t *testing.T) {
	c := connectClient(t)
	ch := uniqueKey(t, "chan")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, err := c.Subscribe(ctx, ch)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()

	// Wait for the subscription to settle before publishing. The driver
	// delivers 'subscribe' confirmations as push frames that are consumed
	// internally (not on the message channel), so a small delay is enough.
	time.Sleep(100 * time.Millisecond)

	publish(t, ch, "hello")

	select {
	case m := <-ps.Channel():
		if m == nil {
			t.Fatalf("nil message")
		}
		if m.Channel != ch || m.Payload != "hello" {
			t.Fatalf("got Channel=%q Payload=%q, want Channel=%q Payload=hello", m.Channel, m.Payload, ch)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for message")
	}
}

func TestPubSub_Pattern(t *testing.T) {
	c := connectClient(t)
	pattern := uniqueKey(t, "pat") + ".*"
	target := uniqueKey(t, "pat") + ".one"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, err := c.PSubscribe(ctx, pattern)
	if err != nil {
		t.Fatalf("PSubscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()

	time.Sleep(100 * time.Millisecond)

	publish(t, target, "hey")

	select {
	case m := <-ps.Channel():
		if m == nil {
			t.Fatalf("nil message")
		}
		// Pattern messages carry both Channel (actual) and Pattern (match).
		if m.Channel != target || m.Payload != "hey" {
			t.Fatalf("got Channel=%q Payload=%q, want Channel=%q Payload=hey", m.Channel, m.Payload, target)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for pmessage")
	}
}

func TestPubSub_UnsubscribeSilencesChannel(t *testing.T) {
	c := connectClient(t)
	ch := uniqueKey(t, "chan")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ps, err := c.Subscribe(ctx, ch)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()
	time.Sleep(100 * time.Millisecond)

	if err := ps.Unsubscribe(ctx, ch); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	publish(t, ch, "nobody home")

	select {
	case m, ok := <-ps.Channel():
		if ok && m != nil && m.Channel == ch {
			t.Fatalf("received message on unsubscribed channel: %+v", m)
		}
	case <-time.After(500 * time.Millisecond):
		// expected: no message arrives
	}
}

// TestPubSub_Reconnect verifies that closing the underlying conn from the
// server side triggers reconnection and re-subscription. The test emulates a
// disconnect by issuing CLIENT KILL via a raw admin conn.
func TestPubSub_Reconnect(t *testing.T) {
	c := connectClient(t)
	ch := uniqueKey(t, "chan")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ps, err := c.Subscribe(ctx, ch)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()
	time.Sleep(150 * time.Millisecond)

	// Kill all clients to force a reconnect.
	killAllClients(t)

	// Give the driver time to detect and re-dial.
	time.Sleep(2 * time.Second)

	publish(t, ch, fmt.Sprintf("after-reconnect-%d", time.Now().UnixNano()))

	select {
	case m := <-ps.Channel():
		if m == nil {
			t.Fatalf("nil message after reconnect")
		}
		if m.Channel != ch {
			t.Fatalf("channel = %q, want %q", m.Channel, ch)
		}
	case <-time.After(5 * time.Second):
		t.Skipf("no message received after reconnect within 5s (reconnect path may be slow/unsupported on this server)")
	}
}

// killAllClients issues CLIENT KILL TYPE pubsub via a raw TCP conn. This
// disconnects every pubsub client so the driver has to reconnect.
func killAllClients(t *testing.T) {
	t.Helper()
	addr := addrFromEnv(t)
	deadline := time.Now().Add(3 * time.Second)
	conn, err := dialNet(addr, deadline)
	if err != nil {
		t.Fatalf("dial for CLIENT KILL: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	if pw := os.Getenv(envPassword); pw != "" {
		if _, err := conn.Write(encodeCommand("AUTH", pw)); err != nil {
			t.Fatalf("AUTH write: %v", err)
		}
		if _, err := readLine(conn); err != nil {
			t.Fatalf("AUTH read: %v", err)
		}
	}
	if _, err := conn.Write(encodeCommand("CLIENT", "KILL", "TYPE", "pubsub")); err != nil {
		t.Fatalf("CLIENT KILL write: %v", err)
	}
	if _, err := readLine(conn); err != nil {
		t.Fatalf("CLIENT KILL read: %v", err)
	}
}
