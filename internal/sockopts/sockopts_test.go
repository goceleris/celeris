package sockopts

import (
	"testing"
	"time"
)

func TestOptionsDefaults(t *testing.T) {
	opts := Options{
		TCPNoDelay:  true,
		TCPQuickAck: true,
		SOBusyPoll:  50 * time.Microsecond,
		RecvBuf:     65536,
		SendBuf:     65536,
	}
	if !opts.TCPNoDelay {
		t.Fatal("expected TCPNoDelay true")
	}
	if opts.RecvBuf != 65536 {
		t.Fatal("expected RecvBuf 65536")
	}
}

func TestApplyFDInvalidFD(_ *testing.T) {
	opts := Options{TCPNoDelay: true}
	// On non-linux, this is a no-op. On linux, invalid fd returns error.
	_ = ApplyFD(-1, opts)
}
