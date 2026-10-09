package resource

import (
	"testing"
	"time"
)

// TestWithDefaultsIsIdempotent pins celeris#594: WithDefaults runs at least
// twice on every start (Server.doPrepare, then the engine constructor, then a
// third time for the adaptive sub-engine), and the -1 "disabled" sentinels are
// carried internally as 0 — the same value a caller uses to ask for the
// default. Before the fix the second pass read the first pass's 0 as "unset"
// and reinstated the default, so ReadHeaderTimeout=-1 arrived at the worker as
// 10s.
//
// The table covers every sentinel field × {-1 (disabled), 0 (default), N
// (explicit)} and asserts the value after 1, 2 and 3 passes.
func TestWithDefaultsIsIdempotent(t *testing.T) {
	const explicit = 3 * time.Second

	type fieldCase struct {
		name string
		set  func(c *Config, d time.Duration)
		get  func(c Config) time.Duration
		def  time.Duration
	}
	fields := []fieldCase{
		{
			name: "ReadTimeout",
			set:  func(c *Config, d time.Duration) { c.ReadTimeout = d },
			get:  func(c Config) time.Duration { return c.ReadTimeout },
			def:  60 * time.Second,
		},
		{
			name: "ReadHeaderTimeout",
			set:  func(c *Config, d time.Duration) { c.ReadHeaderTimeout = d },
			get:  func(c Config) time.Duration { return c.ReadHeaderTimeout },
			def:  10 * time.Second,
		},
		{
			name: "WriteTimeout",
			set:  func(c *Config, d time.Duration) { c.WriteTimeout = d },
			get:  func(c Config) time.Duration { return c.WriteTimeout },
			def:  60 * time.Second,
		},
		{
			name: "IdleTimeout",
			set:  func(c *Config, d time.Duration) { c.IdleTimeout = d },
			get:  func(c Config) time.Duration { return c.IdleTimeout },
			def:  600 * time.Second,
		},
	}

	inputs := []struct {
		name  string
		in    time.Duration
		wantF func(def time.Duration) time.Duration
	}{
		{name: "disabled/-1", in: -1, wantF: func(time.Duration) time.Duration { return 0 }},
		{name: "unset/0", in: 0, wantF: func(def time.Duration) time.Duration { return def }},
		{name: "explicit/3s", in: explicit, wantF: func(time.Duration) time.Duration { return explicit }},
	}

	for _, f := range fields {
		for _, in := range inputs {
			t.Run(f.name+"/"+in.name, func(t *testing.T) {
				want := in.wantF(f.def)
				var c Config
				f.set(&c, in.in)
				for pass := 1; pass <= 3; pass++ {
					c = c.WithDefaults()
					if got := f.get(c); got != want {
						t.Fatalf("pass %d: %s = %v, want %v (input %v)",
							pass, f.name, got, want, in.in)
					}
				}
			})
		}
	}
}

// TestWithDefaultsMaxRequestBodySizeIdempotent covers the same double
// normalisation on the other -1 sentinel field: -1 means "unlimited", carried
// internally as 0, which the second pass used to replace with the 100 MB
// default (celeris#594).
func TestWithDefaultsMaxRequestBodySizeIdempotent(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{name: "unlimited/-1", in: -1, want: 0},
		{name: "unset/0", in: 0, want: 100 << 20},
		{name: "explicit", in: 4096, want: 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{MaxRequestBodySize: tc.in}
			for pass := 1; pass <= 3; pass++ {
				c = c.WithDefaults()
				if c.MaxRequestBodySize != tc.want {
					t.Fatalf("pass %d: MaxRequestBodySize = %d, want %d (input %d)",
						pass, c.MaxRequestBodySize, tc.want, tc.in)
				}
			}
		})
	}
}

// TestWithDefaultsPreservesNonSentinelDefaults guards the fix's blast radius:
// making the pass idempotent must not change any other default, and the
// non-sentinel fields must still survive re-normalisation unchanged.
func TestWithDefaultsPreservesNonSentinelDefaults(t *testing.T) {
	one := Config{}.WithDefaults()
	two := one.WithDefaults()

	checks := []struct {
		name     string
		got      any
		want     any
		gotTwice any
	}{
		{"Addr", one.Addr, ":8080", two.Addr},
		{"MaxFrameSize", one.MaxFrameSize, uint32(1 << 20), two.MaxFrameSize},
		{"InitialWindowSize", one.InitialWindowSize, uint32(1 << 20), two.InitialWindowSize},
		{"MaxConcurrentStreams", one.MaxConcurrentStreams, uint32(100), two.MaxConcurrentStreams},
		{"MaxHeaderBytes", one.MaxHeaderBytes, 16 << 20, two.MaxHeaderBytes},
		{"MaxRequestBodySize", one.MaxRequestBodySize, int64(100 << 20), two.MaxRequestBodySize},
		{"ReadTimeout", one.ReadTimeout, 60 * time.Second, two.ReadTimeout},
		{"ReadHeaderTimeout", one.ReadHeaderTimeout, 10 * time.Second, two.ReadHeaderTimeout},
		{"WriteTimeout", one.WriteTimeout, 60 * time.Second, two.WriteTimeout},
		{"IdleTimeout", one.IdleTimeout, 600 * time.Second, two.IdleTimeout},
		{"EnableH2Upgrade", one.EnableH2Upgrade, true, two.EnableH2Upgrade},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s after one pass = %v, want %v", c.name, c.got, c.want)
		}
		if c.gotTwice != c.got {
			t.Errorf("%s changed on the second pass: %v -> %v", c.name, c.got, c.gotTwice)
		}
	}
	if one.Protocol != two.Protocol || one.Engine != two.Engine {
		t.Errorf("Protocol/Engine changed on the second pass: %v/%v -> %v/%v",
			one.Protocol, one.Engine, two.Protocol, two.Engine)
	}
	if one.Logger == nil || two.Logger == nil {
		t.Error("Logger must be non-nil after WithDefaults")
	}
}

// TestWithDefaultsNegativeAfterNormalisation covers a caller that re-sets a
// timeout to the -1 sentinel on an already-normalised Config (the shape
// Server.doPrepare's configureFn could take): the next pass must still map it
// to the disabled encoding rather than leaking a negative duration into an
// engine, where a negative deadline would expire immediately.
func TestWithDefaultsNegativeAfterNormalisation(t *testing.T) {
	c := Config{}.WithDefaults()
	c.ReadHeaderTimeout = -1
	c.IdleTimeout = -1
	c = c.WithDefaults()
	if c.ReadHeaderTimeout != 0 {
		t.Errorf("ReadHeaderTimeout = %v, want 0 (disabled)", c.ReadHeaderTimeout)
	}
	if c.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0 (disabled)", c.IdleTimeout)
	}
}
