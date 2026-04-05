package jwtparse

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
)

type signingMethodRSA struct {
	alg  string
	hash crypto.Hash
}

// RSA PKCS#1 v1.5 signing method singletons.
var (
	SigningMethodRS256 *signingMethodRSA
	SigningMethodRS384 *signingMethodRSA
	SigningMethodRS512 *signingMethodRSA
)

func init() {
	SigningMethodRS256 = &signingMethodRSA{"RS256", crypto.SHA256}
	SigningMethodRS384 = &signingMethodRSA{"RS384", crypto.SHA384}
	SigningMethodRS512 = &signingMethodRSA{"RS512", crypto.SHA512}

	RegisterSigningMethod(SigningMethodRS256)
	RegisterSigningMethod(SigningMethodRS384)
	RegisterSigningMethod(SigningMethodRS512)
}

func (m *signingMethodRSA) Alg() string { return m.alg }

func (m *signingMethodRSA) Verify(signingInput string, sig []byte, key any) error {
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: RSA verify key must be *rsa.PublicKey", ErrTokenUnverifiable)
	}

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	if err := rsa.VerifyPKCS1v15(pub, m.hash, digest, sig); err != nil {
		return ErrTokenSignatureInvalid
	}
	return nil
}

func (m *signingMethodRSA) Sign(signingInput string, key any) ([]byte, error) {
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: RSA sign key must be *rsa.PrivateKey", ErrTokenUnverifiable)
	}

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	return rsa.SignPKCS1v15(rand.Reader, priv, m.hash, digest)
}
