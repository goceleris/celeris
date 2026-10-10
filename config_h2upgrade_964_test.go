package celeris

import "testing"

// TestToResourceConfig_H2Upgrade is the resolution table for
// Config.EnableH2Upgrade (celeris#964), taken the way the server takes it:
// toResourceConfig, then WithDefaults, which runs again in every engine
// constructor (three times through adaptive), so the value must hold across
// passes. nil keeps the protocol-dependent default; a non-nil value is final,
// including &false on Auto, which WithDefaults used to turn back into true.
func TestToResourceConfig_H2Upgrade(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		protocol Protocol
		enable   *bool
		want     bool
	}{
		{"auto/nil enables", Auto, nil, true},
		{"auto/true enables", Auto, &yes, true},
		{"auto/false disables", Auto, &no, false},
		{"zero-protocol/nil enables", Protocol(0), nil, true},
		{"zero-protocol/false disables", Protocol(0), &no, false},
		{"h2c/nil disabled", H2C, nil, false},
		{"h2c/true enables", H2C, &yes, true},
		{"h2c/false disabled", H2C, &no, false},
		{"http1/nil disabled", HTTP1, nil, false},
		{"http1/true enables", HTTP1, &yes, true},
		{"http1/false disabled", HTTP1, &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := Config{Protocol: tc.protocol, EnableH2Upgrade: tc.enable}.toResourceConfig()
			if rc.EnableH2Upgrade != tc.want {
				t.Fatalf("toResourceConfig: EnableH2Upgrade = %v, want %v", rc.EnableH2Upgrade, tc.want)
			}
			for pass := 1; pass <= 3; pass++ {
				rc = rc.WithDefaults()
				if rc.EnableH2Upgrade != tc.want {
					t.Fatalf("WithDefaults pass %d: EnableH2Upgrade = %v, want %v", pass, rc.EnableH2Upgrade, tc.want)
				}
			}
		})
	}
}
