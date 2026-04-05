package jwtparse

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"math/big"
)

type signingMethodECDSA struct {
	alg      string
	hash     crypto.Hash
	keySize  int
	curveBit int
}

// ECDSA signing method singletons.
var (
	SigningMethodES256 *signingMethodECDSA
	SigningMethodES384 *signingMethodECDSA
	SigningMethodES512 *signingMethodECDSA
)

func init() {
	SigningMethodES256 = &signingMethodECDSA{"ES256", crypto.SHA256, 32, 256}
	SigningMethodES384 = &signingMethodECDSA{"ES384", crypto.SHA384, 48, 384}
	SigningMethodES512 = &signingMethodECDSA{"ES512", crypto.SHA512, 66, 521}

	RegisterSigningMethod(SigningMethodES256)
	RegisterSigningMethod(SigningMethodES384)
	RegisterSigningMethod(SigningMethodES512)
}

func (m *signingMethodECDSA) Alg() string { return m.alg }

func (m *signingMethodECDSA) Verify(signingInput string, sig []byte, key any) error {
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: ECDSA verify key must be *ecdsa.PublicKey", ErrTokenUnverifiable)
	}

	if len(sig) != 2*m.keySize {
		return ErrTokenSignatureInvalid
	}

	r := new(big.Int).SetBytes(sig[:m.keySize])
	s := new(big.Int).SetBytes(sig[m.keySize:])

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	if !ecdsa.Verify(pub, digest, r, s) {
		return ErrTokenSignatureInvalid
	}
	return nil
}

func (m *signingMethodECDSA) Sign(signingInput string, key any) ([]byte, error) {
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: ECDSA sign key must be *ecdsa.PrivateKey", ErrTokenUnverifiable)
	}

	h := m.hash.New()
	h.Write(stringToBytes(signingInput))
	digest := h.Sum(nil)

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
	if err != nil {
		return nil, err
	}

	curveBits := priv.Curve.Params().BitSize
	keyBytes := curveBits / 8
	if curveBits%8 > 0 {
		keyBytes++
	}

	rBytes := r.Bytes()
	sBytes := s.Bytes()

	out := make([]byte, 2*keyBytes)
	copy(out[keyBytes-len(rBytes):keyBytes], rBytes)
	copy(out[2*keyBytes-len(sBytes):], sBytes)

	return out, nil
}

// curveForBit returns the curve for the given bit size.
func curveForBit(bits int) elliptic.Curve {
	switch bits {
	case 256:
		return elliptic.P256()
	case 384:
		return elliptic.P384()
	case 521:
		return elliptic.P521()
	default:
		return nil
	}
}

// GenerateECDSAKey generates an ECDSA key pair for the given signing method. For testing only.
func GenerateECDSAKey(m *signingMethodECDSA) (*ecdsa.PrivateKey, error) {
	c := curveForBit(m.curveBit)
	if c == nil {
		return nil, fmt.Errorf("unsupported curve bit size: %d", m.curveBit)
	}
	return ecdsa.GenerateKey(c, rand.Reader)
}
