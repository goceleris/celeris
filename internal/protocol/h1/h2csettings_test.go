package h1

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDecodeHTTP2Settings(t *testing.T) {
	// Build a representative settings payload: two entries (id+value pairs, 6 bytes each).
	payload := []byte{
		0x00, 0x03, 0x00, 0x00, 0x00, 0x64, // SETTINGS_MAX_CONCURRENT_STREAMS = 100
		0x00, 0x04, 0x00, 0x01, 0x00, 0x00, // SETTINGS_INITIAL_WINDOW_SIZE = 65536
	}
	rawURL := base64.RawURLEncoding.EncodeToString(payload)
	padded := base64.URLEncoding.EncodeToString(payload)

	cases := []struct {
		name    string
		in      string
		wantErr bool
		want    []byte
	}{
		// An empty value is a SETTINGS payload with no entries (RFC 7540
		// 6.5): valid, and the upgrade goes ahead (celeris#889, item 43).
		{"empty is a valid empty payload", "", false, nil},
		{"raw url encoded", rawURL, false, payload},
		{"padded url encoded", padded, false, payload},
		{"invalid base64", "!!!!", true, nil},
		{"standard alphabet is not base64url", "AAMA+/ABkAAQAoAAAAAIAAAAA", true, nil},
		{"wrong length (5 bytes)", base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4, 5}), true, nil},
		{"wrong length (7 bytes)", base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6, 7}), true, nil},
		{"two valid values joined", rawURL + "," + rawURL, true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeHTTP2Settings(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (decoded %x)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %x, want %x", got, tc.want)
			}
		})
	}
}
