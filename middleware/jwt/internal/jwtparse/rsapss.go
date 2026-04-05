package jwtparse

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
)

type signingMethodRSAPSS struct {
	alg  string
	hash crypto.Hash
}

// RSA-PSS signing method singletons.
var (
	SigningMethodPS256 *signingMethodRSAPSS
	SigningMethodPS384 *signingMethodRSAPSS
	SigningMethodPS512 *signingMethodRSAPSS
)

func init() {
	SigningMethodPS256 = &signingMethodRSAPSS{"PS256", crypto.SHA256}
	SigningMethodPS384 = &signingMethodRSAPSS{"PS384", crypto.SHA384}
	SigningMethodPS512 = &signingMethodRSAPSS{"PS512", crypto.SHA512}

	RegisterSigningMethod(SigningMethodPS256)
	RegisterSigningMethod(SigningMethodPS384)
	RegisterSigningMethod(SigningMethodPS512)
}

func (m *signingMethodRSAPSS) Alg() string { return m.alg }

func (m *signingMethodRSAPSS) Verify(signingInput string, sig []byte, key any) error {
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: RSA-PSS verify key must be *rsa.PublicKey", ErrTokenUnverifiable)
	}

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}
	if err := rsa.VerifyPSS(pub, m.hash, digest, sig, opts); err != nil {
		return ErrTokenSignatureInvalid
	}
	return nil
}

func (m *signingMethodRSAPSS) Sign(signingInput string, key any) ([]byte, error) {
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: RSA-PSS sign key must be *rsa.PrivateKey", ErrTokenUnverifiable)
	}

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}
	return rsa.SignPSS(rand.Reader, priv, m.hash, digest, opts)
}
