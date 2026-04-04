package jwtparse

import "sync"

// SigningMethod defines the interface for JWT signing algorithms.
type SigningMethod interface {
	Alg() string
	Verify(signingInput string, sig []byte, key any) error
	Sign(signingInput string, key any) ([]byte, error)
}

var methods sync.Map

// RegisterSigningMethod registers a signing method in the global registry.
func RegisterSigningMethod(m SigningMethod) {
	methods.Store(m.Alg(), m)
}

// GetSigningMethod returns a registered signing method by algorithm name.
func GetSigningMethod(alg string) (SigningMethod, bool) {
	v, ok := methods.Load(alg)
	if !ok {
		return nil, false
	}
	return v.(SigningMethod), true
}
