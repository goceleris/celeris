package resource

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
)

func TestValidateValidConfig(t *testing.T) {
	c := Config{
		Addr:              ":8080",
		Engine:            engine.Std,
		MaxFrameSize:      16384,
		InitialWindowSize: 65535,
	}
	errs := c.Validate()
	if len(errs) > 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateEmptyConfig(t *testing.T) {
	c := Config{}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "addr") {
			t.Error("empty addr should not produce error")
		}
	}
}

func TestValidateInvalidPort(t *testing.T) {
	c := Config{Addr: ":99999"}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "port") || strings.Contains(e.Error(), "65535") {
			found = true
		}
	}
	if !found {
		t.Error("expected port validation error for :99999")
	}
}

func TestValidateInvalidAddr(t *testing.T) {
	c := Config{Addr: "not-valid"}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "invalid addr") {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid addr error")
	}
}

func TestValidateMaxFrameSizeTooSmall(t *testing.T) {
	c := Config{MaxFrameSize: 1000}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxFrameSize") {
			found = true
		}
	}
	if !found {
		t.Error("expected MaxFrameSize validation error")
	}
}

func TestValidateMaxFrameSizeTooLarge(t *testing.T) {
	c := Config{MaxFrameSize: 16777216}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxFrameSize") {
			found = true
		}
	}
	if !found {
		t.Error("expected MaxFrameSize validation error for too large value")
	}
}

func TestValidateInitialWindowSizeOverflow(t *testing.T) {
	c := Config{InitialWindowSize: 2147483648}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "initialWindowSize") {
			found = true
		}
	}
	if !found {
		t.Error("expected InitialWindowSize validation error")
	}
}

func TestValidateNegativeTimeouts(t *testing.T) {
	c := Config{
		ReadTimeout:  -1 * time.Second,
		WriteTimeout: -1 * time.Second,
		IdleTimeout:  -1 * time.Second,
	}
	errs := c.Validate()
	timeoutErrors := 0
	for _, e := range errs {
		if strings.Contains(e.Error(), "imeout") {
			timeoutErrors++
		}
	}
	if timeoutErrors != 3 {
		t.Errorf("expected 3 timeout errors, got %d", timeoutErrors)
	}
}

func TestValidateWorkersBelowMin(t *testing.T) {
	c := Config{
		Resources: Resources{Workers: 1},
	}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "workers") {
			found = true
		}
	}
	if !found {
		t.Error("expected Workers validation error for Workers=1")
	}
}

func TestValidateBufferSizeBelowMin(t *testing.T) {
	c := Config{
		Resources: Resources{BufferSize: 1024},
	}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "bufferSize") {
			found = true
		}
	}
	if !found {
		t.Error("expected BufferSize validation error for BufferSize=1024")
	}
}

func TestValidateEngineRequiresLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this test only runs on non-Linux")
	}

	tests := []engine.EngineType{engine.IOUring, engine.Epoll}
	for _, eng := range tests {
		c := Config{Engine: eng}
		errs := c.Validate()
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "requires Linux") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected Linux requirement error for engine %s", eng)
		}
	}
}

func TestValidateStdEngineOnAnyPlatform(t *testing.T) {
	c := Config{Engine: engine.Std}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires Linux") {
			t.Error("Std engine should not require Linux")
		}
	}
}

func TestWithDefaults(t *testing.T) {
	c := Config{}
	d := c.WithDefaults()

	if d.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", d.Addr)
	}
	if d.MaxFrameSize != 16384 {
		t.Errorf("MaxFrameSize = %d, want 16384", d.MaxFrameSize)
	}
	if d.InitialWindowSize != 65535 {
		t.Errorf("InitialWindowSize = %d, want 65535", d.InitialWindowSize)
	}
	if d.MaxConcurrentStreams != 100 {
		t.Errorf("MaxConcurrentStreams = %d, want 100", d.MaxConcurrentStreams)
	}
	if d.MaxHeaderBytes != 16<<20 {
		t.Errorf("MaxHeaderBytes = %d, want %d", d.MaxHeaderBytes, 16<<20)
	}
	if d.Logger == nil {
		t.Error("Logger should not be nil after WithDefaults")
	}
	if d.ReadTimeout != 60*time.Second {
		t.Errorf("ReadTimeout = %v, want 1m0s", d.ReadTimeout)
	}
	if d.WriteTimeout != 60*time.Second {
		t.Errorf("WriteTimeout = %v, want 1m0s", d.WriteTimeout)
	}
	if d.IdleTimeout != 600*time.Second {
		t.Errorf("IdleTimeout = %v, want 10m0s", d.IdleTimeout)
	}
}

func TestWithDefaultsPreservesExisting(t *testing.T) {
	c := Config{
		Addr:         ":9090",
		MaxFrameSize: 32768,
		ReadTimeout:  5 * time.Second,
	}
	d := c.WithDefaults()
	if d.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090 (should preserve)", d.Addr)
	}
	if d.MaxFrameSize != 32768 {
		t.Errorf("MaxFrameSize = %d, want 32768 (should preserve)", d.MaxFrameSize)
	}
	if d.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %v, want 5s (should preserve)", d.ReadTimeout)
	}
}

func TestValidateMaxHeaderBytesTooSmall(t *testing.T) {
	c := Config{MaxHeaderBytes: 2048}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxHeaderBytes") {
			found = true
		}
	}
	if !found {
		t.Error("expected maxHeaderBytes validation error for value 2048")
	}
}

func TestValidateMaxHeaderBytesAtMinimum(t *testing.T) {
	c := Config{MaxHeaderBytes: 4096}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxHeaderBytes") {
			t.Error("maxHeaderBytes=4096 should not produce error")
		}
	}
}

func TestValidateMaxHeaderBytesZeroIsValid(t *testing.T) {
	c := Config{MaxHeaderBytes: 0}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxHeaderBytes") {
			t.Error("maxHeaderBytes=0 means use default and should not produce error")
		}
	}
}

func TestValidateMaxConcurrentStreamsZeroIsValid(t *testing.T) {
	c := Config{MaxConcurrentStreams: 0}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxConcurrentStreams") {
			t.Error("maxConcurrentStreams=0 means use default and should not produce error")
		}
	}
}

func TestValidateMaxConcurrentStreamsTooLarge(t *testing.T) {
	c := Config{MaxConcurrentStreams: 0x80000000}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "maxConcurrentStreams") {
			found = true
		}
	}
	if !found {
		t.Error("expected maxConcurrentStreams validation error for value > 2147483647")
	}
}

func TestValidateZeroPort(t *testing.T) {
	c := Config{Addr: ":0"}
	errs := c.Validate()
	for _, e := range errs {
		if strings.Contains(e.Error(), "port") {
			t.Error("port 0 should be allowed for OS-assigned port, got error:", e)
		}
	}
}

func TestValidateNegativePort(t *testing.T) {
	c := Config{Addr: ":-1"}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "port") {
			found = true
		}
	}
	if !found {
		t.Error("expected port validation error for :-1")
	}
}

func TestWithDefaults_EnableH2Upgrade(t *testing.T) {
	cases := []struct {
		name     string
		in       Config
		wantH2Up bool
	}{
		{
			name:     "default protocol enables upgrade",
			in:       Config{},
			wantH2Up: true,
		},
		{
			name:     "explicit Auto enables upgrade",
			in:       Config{Protocol: engine.Auto},
			wantH2Up: true,
		},
		{
			name:     "HTTP1 does not get upgrade by default",
			in:       Config{Protocol: engine.HTTP1},
			wantH2Up: false,
		},
		{
			name:     "H2C does not get upgrade by default",
			in:       Config{Protocol: engine.H2C},
			wantH2Up: false,
		},
		{
			name:     "explicit EnableH2Upgrade true on HTTP1 is preserved",
			in:       Config{Protocol: engine.HTTP1, EnableH2Upgrade: true},
			wantH2Up: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.WithDefaults()
			if got.EnableH2Upgrade != tc.wantH2Up {
				t.Fatalf("EnableH2Upgrade = %v, want %v", got.EnableH2Upgrade, tc.wantH2Up)
			}
		})
	}
}
