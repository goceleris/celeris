package conn

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
)

// ErrUpgradeH2C is returned by ProcessH1 after it has written the 101
// Switching Protocols response in reply to an RFC 7540 §3.2 h2c upgrade
// request. The engine must NOT treat this as an error: instead it must
// read state.UpgradeInfo and switch the connection to H2.
//
// The original handler has NOT been invoked; it will be dispatched on H2
// stream 1 by NewH2StateFromUpgrade once the H2 stack is up.
var ErrUpgradeH2C = errors.New("celeris: h2c upgrade requested")

// upgradeInfoPool recycles UpgradeInfo structs so each h2c upgrade doesn't
// pay the wrapper allocation. Acquire via acquireUpgradeInfo, release via
// ReleaseUpgradeInfo after switchToH2 has finished consuming it.
var upgradeInfoPool = sync.Pool{
	New: func() any { return &UpgradeInfo{} },
}

func acquireUpgradeInfo() *UpgradeInfo {
	return upgradeInfoPool.Get().(*UpgradeInfo)
}

// ReleaseUpgradeInfo resets every field and returns the struct to the pool.
// Aliased slices (Body, Remaining) keep their source buffers alive via their
// slice references, so we MUST nil them out before returning to the pool or
// the next acquirer's value will pin stale memory.
func ReleaseUpgradeInfo(info *UpgradeInfo) {
	if info == nil {
		return
	}
	info.Settings = nil
	info.Method = ""
	info.URI = ""
	info.Body = nil
	info.Remaining = nil
	// Reuse Headers backing array where possible: a clean-up loop zeroing
	// each entry is cheaper than realloc on the next upgrade. The slice
	// itself is truncated to zero-length but capacity is retained.
	for i := range info.Headers {
		info.Headers[i] = [2]string{}
	}
	info.Headers = info.Headers[:0]
	upgradeInfoPool.Put(info)
}

// UpgradeInfo carries the state needed to switch an H1 connection to H2
// mid-stream. It is populated by ProcessH1 on an h2c upgrade, then
// consumed by the engine's switchToH2 path.
type UpgradeInfo struct {
	// Settings is the base64url-decoded HTTP2-Settings payload. Each entry
	// is a 6-byte pair (u16 identifier + u32 value); length is always a
	// multiple of 6 (DecodeHTTP2Settings rejects others).
	Settings []byte
	// Method is the H1 request method (used to build the H2 :method
	// pseudo-header).
	Method string
	// URI is the raw request URI as sent on the request line (used for
	// :path).
	URI string
	// Headers are the request's non-hop-by-hop headers, lowercased,
	// excluding Upgrade/Connection/HTTP2-Settings (which are consumed by
	// the upgrade mechanism itself and do not belong on H2 stream 1).
	Headers [][2]string
	// Body holds the request body (may be empty).
	Body []byte
	// Remaining holds the bytes AFTER the H1 request in the recv buffer.
	// May contain 0 bytes, a partial H2 client preface, a full preface, or
	// preface + additional frames. The engine feeds this to ProcessH2
	// synchronously after switchToH2 so no data is dropped at the seam.
	Remaining []byte
}

// DecodeHTTP2Settings base64url-decodes the HTTP2-Settings header value.
// RFC 7540 §3.2.1 specifies base64url without padding, but some clients
// include padding — accept both.
//
// Validation: the decoded payload length must be a multiple of 6 (one
// SETTINGS entry = u16 id + u32 value). Any other length indicates a
// malformed payload.
func DecodeHTTP2Settings(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("empty HTTP2-Settings")
	}
	// Try RFC-preferred unpadded URL encoding first.
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Fall back to padded URL encoding for lenient clients.
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("base64url decode: %w", err)
		}
	}
	if len(b)%6 != 0 {
		return nil, fmt.Errorf("HTTP2-Settings payload length %d not a multiple of 6", len(b))
	}
	return b, nil
}
