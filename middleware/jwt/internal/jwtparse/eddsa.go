package jwtparse

import (
	"crypto/ed25519"
	"fmt"
)

type signingMethodEdDSA struct{}

// SigningMethodEdDSA is the EdDSA signing method singleton.
var SigningMethodEdDSA *signingMethodEdDSA

func init() {
	SigningMethodEdDSA = &signingMethodEdDSA{}
	RegisterSigningMethod(SigningMethodEdDSA)
}

func (m *signingMethodEdDSA) Alg() string { return "EdDSA" }

func (m *signingMethodEdDSA) Verify(signingInput string, sig []byte, key any) error {
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("%w: EdDSA verify key must be ed25519.PublicKey", ErrTokenUnverifiable)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: EdDSA key size %d, want %d", ErrTokenUnverifiable, len(pub), ed25519.PublicKeySize)
	}

	if !ed25519.Verify(pub, stringToBytes(signingInput), sig) {
		return ErrTokenSignatureInvalid
	}
	return nil
}

func (m *signingMethodEdDSA) Sign(signingInput string, key any) ([]byte, error) {
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: EdDSA sign key must be ed25519.PrivateKey", ErrTokenUnverifiable)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: EdDSA key size %d, want %d", ErrTokenUnverifiable, len(priv), ed25519.PrivateKeySize)
	}

	return ed25519.Sign(priv, stringToBytes(signingInput)), nil
}
