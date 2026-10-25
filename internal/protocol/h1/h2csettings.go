package h1

import (
	"encoding/base64"
	"fmt"
)

// DecodeHTTP2Settings decodes the value of the HTTP2-Settings header field of
// an h2c upgrade request (RFC 7540 section 3.2.1): the payload of a SETTINGS
// frame, base64url-encoded. It is the one test every engine applies to the
// value, so that they upgrade on the same requests: a value that does not
// decode is not a request to upgrade, and is served as HTTP/1.1.
//
// RFC 7540 section 3.2.1 specifies base64url without padding, but some clients
// include padding: both are accepted.
//
// The empty value decodes to an empty payload (nil, no error): a SETTINGS
// frame with no entries (RFC 7540 section 6.5), whose settings are all the
// protocol defaults. It upgrades.
//
// Validation: the decoded payload length must be a multiple of 6 (one
// SETTINGS entry = u16 id + u32 value). Any other length indicates a
// malformed payload.
func DecodeHTTP2Settings(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
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
