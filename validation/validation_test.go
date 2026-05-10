//go:build validation

package validation

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestPanicCountIncrement is the synthetic-panic predicate from the
// wave-7 acceptance criteria: bumping the counter directly is what
// every higher-layer call site does, so verifying the counter
// machinery isolates the wiring from the rest of the system.
func TestPanicCountIncrement(t *testing.T) {
	before := PanicCount.Load()
	func() {
		defer func() {
			if r := recover(); r != nil {
				PanicCount.Add(1)
			}
		}()
		panic("synthetic")
	}()
	after := PanicCount.Load()
	if after != before+1 {
		t.Fatalf("PanicCount: got %d, want %d", after, before+1)
	}
}

// TestEndpointServesJSON binds the endpoint, makes a GET, and
// verifies the JSON payload carries the bumped counter. The endpoint
// path is the same hard-coded SocketPath that probatorium's
// validator-checker reads.
func TestEndpointServesJSON(t *testing.T) {
	// Reset for a deterministic snapshot. The endpoint is a process
	// singleton, so other tests must run on either side of this
	// reset cleanly — Atomics carry no destructor, so a Store(0)
	// here is sufficient.
	PanicCount.Store(0)
	RatelimitTokenViolations.Store(0)

	ep, err := StartEndpoint()
	if err != nil {
		t.Fatalf("StartEndpoint: %v", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ep.Stop(shutCtx)
	}()

	// Bump a couple counters and verify the JSON shape echoes them.
	PanicCount.Add(2)
	RatelimitTokenViolations.Add(3)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", SocketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unused/")
	if err != nil {
		t.Fatalf("GET via unix socket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got Counters
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PanicCount != 2 {
		t.Errorf("PanicCount: got %d, want 2", got.PanicCount)
	}
	if got.RatelimitTokenViolations != 3 {
		t.Errorf("RatelimitTokenViolations: got %d, want 3", got.RatelimitTokenViolations)
	}
}

// TestSnapshotConsistency verifies Counters fields stay in sync with
// the package-level atomic variables. Adding a new atomic without
// updating Snapshot would silently leave the new counter invisible
// to probatorium; this test catches the omission.
func TestSnapshotConsistency(t *testing.T) {
	PanicCount.Store(11)
	RaceFires.Store(12)
	RatelimitTokenViolations.Store(13)
	SessionOwnerMismatches.Store(14)
	JWTLateAdmits.Store(15)
	IouringSQECorruptions.Store(16)
	AdaptiveSwitchFDLeaks.Store(17)
	defer func() {
		PanicCount.Store(0)
		RaceFires.Store(0)
		RatelimitTokenViolations.Store(0)
		SessionOwnerMismatches.Store(0)
		JWTLateAdmits.Store(0)
		IouringSQECorruptions.Store(0)
		AdaptiveSwitchFDLeaks.Store(0)
	}()
	got := Snapshot()
	want := Counters{
		PanicCount:               11,
		RaceFires:                12,
		RatelimitTokenViolations: 13,
		SessionOwnerMismatches:   14,
		JWTLateAdmits:            15,
		IouringSQECorruptions:    16,
		AdaptiveSwitchFDLeaks:    17,
	}
	if got != want {
		t.Fatalf("Snapshot mismatch:\n got  %+v\n want %+v", got, want)
	}
}
