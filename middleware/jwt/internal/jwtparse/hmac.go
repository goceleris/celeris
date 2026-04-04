package jwtparse

import (
	"crypto"
	"crypto/hmac"
	"crypto/subtle"
	"fmt"
	"hash"
	"sync"
	"unsafe"
)

type signingMethodHMAC struct {
	alg  string
	hash crypto.Hash
	// pool caches *hmacEntry instances to amortize HMAC hasher creation.
	// Each entry contains the hash.Hash and the key it was created with.
	// On Get, we verify the pooled entry's key matches via constant-time
	// compare; on mismatch we discard it and create a new hasher. This
	// is safe because sync.Pool items are per-P and have no cross-goroutine
	// sharing guarantees -- the pool only provides a performance hint.
	pool sync.Pool
}

type hmacEntry struct {
	mac hash.Hash
	key []byte // key used to create this hasher
}

// HMAC-SHA signing method singletons.
var (
	SigningMethodHS256 *signingMethodHMAC
	SigningMethodHS384 *signingMethodHMAC
	SigningMethodHS512 *signingMethodHMAC
)

func init() {
	SigningMethodHS256 = &signingMethodHMAC{alg: "HS256", hash: crypto.SHA256}
	SigningMethodHS384 = &signingMethodHMAC{alg: "HS384", hash: crypto.SHA384}
	SigningMethodHS512 = &signingMethodHMAC{alg: "HS512", hash: crypto.SHA512}

	RegisterSigningMethod(SigningMethodHS256)
	RegisterSigningMethod(SigningMethodHS384)
	RegisterSigningMethod(SigningMethodHS512)
}

func (m *signingMethodHMAC) Alg() string { return m.alg }

func (m *signingMethodHMAC) getHMAC(key []byte) *hmacEntry {
	if v := m.pool.Get(); v != nil {
		e := v.(*hmacEntry)
		if len(e.key) == len(key) && subtle.ConstantTimeCompare(e.key, key) == 1 {
			e.mac.Reset()
			return e
		}
	}
	mac := hmac.New(func() hash.Hash { return m.hash.New() }, key)
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return &hmacEntry{mac: mac, key: keyCopy}
}

func (m *signingMethodHMAC) putHMAC(e *hmacEntry) {
	m.pool.Put(e)
}

func (m *signingMethodHMAC) Verify(signingInput string, sig []byte, key any) error {
	keyBytes, ok := key.([]byte)
	if !ok {
		return fmt.Errorf("%w: HMAC key must be []byte", ErrTokenUnverifiable)
	}
	if len(keyBytes) == 0 {
		return fmt.Errorf("%w: HMAC key is empty", ErrTokenUnverifiable)
	}

	entry := m.getHMAC(keyBytes)
	entry.mac.Write(stringToBytes(signingInput))

	var buf [64]byte
	expected := entry.mac.Sum(buf[:0])
	m.putHMAC(entry)

	if !hmac.Equal(sig, expected) {
		return ErrTokenSignatureInvalid
	}
	return nil
}

func (m *signingMethodHMAC) Sign(signingInput string, key any) ([]byte, error) {
	keyBytes, ok := key.([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: HMAC key must be []byte", ErrTokenUnverifiable)
	}
	if len(keyBytes) == 0 {
		return nil, fmt.Errorf("%w: HMAC key is empty", ErrTokenUnverifiable)
	}

	entry := m.getHMAC(keyBytes)
	entry.mac.Write(stringToBytes(signingInput))
	result := entry.mac.Sum(nil)
	m.putHMAC(entry)
	return result, nil
}

// stringToBytes converts a string to []byte without copying, using
// unsafe.Slice on the string's underlying data pointer. This is safe
// because the returned slice is only used for read-only operations
// (HMAC write, hash write, ed25519.Verify). The slice MUST NOT be
// modified, as the underlying memory belongs to an immutable Go string.
//
// This avoids a heap allocation per Verify/Sign call on the signing
// input, which is a substring of the original token string.
func stringToBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
