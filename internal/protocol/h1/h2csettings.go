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
// RFC 7540 section 3.2.1 specifies base64url without the trailing '='. A payload
// of whole 6-byte entries is 8n characters and never needs one, so padded input
// is not a valid payload whatever it decodes to, and is not accepted.
//
// The empty value decodes to an empty payload (nil, no error): a SETTINGS
// frame with no entries (RFC 7540 section 6.5), whose settings are all the
// protocol defaults. It upgrades. That is a deliberate deviation from the
// grammar: HTTP2-Settings is a token68, and a token68 has at least one
// character, so strictly an empty value is not one. RFC 7540 3.2.1 has its MUST
// NOT only for a missing or a repeated header, an empty SETTINGS frame is
// valid, and std (as x/net's h2c package) has always upgraded on it, so the
// four engines converge on that (celeris#889). Refusing it is this branch.
//
// Validation: the decoded payload length must be a multiple of 6 (one
// SETTINGS entry = u16 id + u32 value). Any other length indicates a
// malformed payload.
func DecodeHTTP2Settings(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64url decode: %w", err)
	}
	if len(b)%6 != 0 {
		return nil, fmt.Errorf("HTTP2-Settings payload length %d not a multiple of 6", len(b))
	}
	return b, nil
}
