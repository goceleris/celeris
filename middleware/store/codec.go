package store

import "encoding/json"

// EncodeJSON returns the JSON encoding of v. Provided for adapters that
// need to persist structured payloads (session data maps) through the
// byte-level [KV] surface.
func EncodeJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// DecodeJSON unmarshals data into v. Convenience wrapper around json.Unmarshal
// for symmetry with [EncodeJSON].
func DecodeJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
