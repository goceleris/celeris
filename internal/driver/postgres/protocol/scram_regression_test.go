package protocol

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestSCRAMTamperedServerSignature ensures a bad server signature surfaces as
// an explicit error. Silent success here would let a MITM complete auth
// without proving it knows the password. (PG-11)
func TestSCRAMTamperedServerSignature(t *testing.T) {
	// Build an SCRAM client and drive it through a normal server-first so it
	// derives saltedPassword + authMessage.
	cli := &scramClient{username: "user", password: "pencil",
		clientNonce: []byte("rOprNGfwEbeRWgbNEkqO")}
	_ = cli.ClientFirst()

	serverFirst := []byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if _, err := cli.HandleServerFirst(serverFirst); err != nil {
		t.Fatal(err)
	}

	// Genuine server signature for this exchange (taken from RFC 7677 §3).
	genuine, err := base64.StdEncoding.DecodeString("6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte to simulate a tampering MITM or a misbehaving server.
	tampered := append([]byte(nil), genuine...)
	tampered[0] ^= 0x01
	sff := []byte("v=" + base64.StdEncoding.EncodeToString(tampered))

	if err := cli.HandleServerFinal(sff); err == nil {
		t.Fatal("expected auth failure on tampered server signature")
	} else if !strings.Contains(err.Error(), "server signature mismatch") {
		t.Fatalf("wrong error: %v", err)
	}

	// And the genuine signature must still pass.
	sff = []byte("v=" + base64.StdEncoding.EncodeToString(genuine))
	if err := cli.HandleServerFinal(sff); err != nil {
		t.Fatalf("genuine signature rejected: %v", err)
	}
}
