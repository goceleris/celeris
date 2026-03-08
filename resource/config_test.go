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
		if strings.Contains(e.Error(), "MaxFrameSize") {
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
		if strings.Contains(e.Error(), "MaxFrameSize") {
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
		if strings.Contains(e.Error(), "InitialWindowSize") {
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
		if strings.Contains(e.Error(), "Timeout") {
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
		if strings.Contains(e.Error(), "Workers") {
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
		if strings.Contains(e.Error(), "BufferSize") {
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
	if d.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want %d", d.MaxHeaderBytes, 1<<20)
	}
	if d.Logger == nil {
		t.Error("Logger should not be nil after WithDefaults")
	}
	if d.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", d.ReadTimeout)
	}
	if d.WriteTimeout != 30*time.Second {
		t.Errorf("WriteTimeout = %v, want 30s", d.WriteTimeout)
	}
	if d.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", d.IdleTimeout)
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

func TestValidateZeroPort(t *testing.T) {
	c := Config{Addr: ":0"}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "port") {
			found = true
		}
	}
	if !found {
		t.Error("expected port validation error for :0")
	}
}
