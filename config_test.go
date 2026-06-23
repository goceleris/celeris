package celeris

import "testing"

// TestMemoryLimitBytesWiring pins that the public Config.MemoryLimitBytes
// reaches resource.Resources through toResourceConfig — without it the knob
// is unreachable and SetMemoryLimit never fires (the B1.1 wiring gap).
func TestMemoryLimitBytesWiring(t *testing.T) {
	const want = 512 << 20
	rc := Config{MemoryLimitBytes: want}.toResourceConfig()
	if rc.Resources.MemoryLimitBytes != want {
		t.Fatalf("MemoryLimitBytes not wired: got %d, want %d", rc.Resources.MemoryLimitBytes, want)
	}
	// 0 (unset) must pass through as 0 so celeris never touches the GC implicitly.
	if rc := (Config{}).toResourceConfig(); rc.Resources.MemoryLimitBytes != 0 {
		t.Fatalf("unset MemoryLimitBytes must map to 0, got %d", rc.Resources.MemoryLimitBytes)
	}
}

func TestDeriveMemoryLimitRoot(t *testing.T) {
	const mib = 1 << 20
	if got := DeriveMemoryLimit(4); got != 256*mib { // 4*32=128 MiB < 256 floor
		t.Fatalf("DeriveMemoryLimit(4) = %d, want %d", got, 256*mib)
	}
	if got := DeriveMemoryLimit(16); got != 512*mib {
		t.Fatalf("DeriveMemoryLimit(16) = %d, want %d", got, 512*mib)
	}
}
