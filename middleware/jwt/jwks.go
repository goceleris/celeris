package jwt

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
	"github.com/goceleris/celeris/middleware/store"
)

type jwksFetcher struct {
	url       string
	client    *http.Client
	mu        sync.RWMutex
	keys      map[string]any // kid -> public key
	lastFetch time.Time
	refresh   time.Duration
	refreshMu sync.Mutex // guards fetch; only one goroutine fetches at a time

	// cache is an optional shared JWKS response cache; when non-nil,
	// fetch() checks the cache before issuing HTTP GET and writes back
	// on success. Cache operations are best-effort; any error from the
	// cache layer is logged and non-fatal.
	cache store.KV
}

// cacheKey returns the store.KV key used for this fetcher's JWKS URL.
// Hashing the URL avoids embedding secrets/tokens that may be present
// in query strings and keeps keys bounded in length.
func (f *jwksFetcher) cacheKey() string {
	h := sha256.Sum256([]byte(f.url))
	return "jwks:" + hex.EncodeToString(h[:])
}

func newJWKSFetcher(url string, refresh time.Duration) *jwksFetcher {
	return &jwksFetcher{
		url:     url,
		refresh: refresh,
		keys:    make(map[string]any),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Preload synchronously fetches the JWKS keyset so the first request
// does not block on a network call. Returns an error if the fetch fails,
// but the fetcher remains usable (lazy fallback on first request).
func (f *jwksFetcher) Preload() error {
	return f.fetch()
}

func (f *jwksFetcher) keyFunc(t *jwtparse.Token) (any, error) {
	kid := t.Header.Kid

	f.mu.RLock()
	key, ok := f.keys[kid]
	stale := time.Since(f.lastFetch) > f.refresh
	f.mu.RUnlock()

	if ok && !stale {
		return key, nil
	}

	// Try to acquire refresh lock (non-blocking).
	if f.refreshMu.TryLock() {
		defer f.refreshMu.Unlock()
		// Double-check under lock.
		f.mu.RLock()
		stale = time.Since(f.lastFetch) > f.refresh
		f.mu.RUnlock()
		if stale {
			if err := f.fetch(); err != nil {
				log.Printf("jwt: JWKS refresh from %s failed: %v", f.url, err)
				// Update lastFetch even on error to prevent a retry storm;
				// stale keys remain available as a fallback.
				f.mu.Lock()
				f.lastFetch = time.Now()
				f.mu.Unlock()
			}
		}
	}

	f.mu.RLock()
	key, ok = f.keys[kid]
	f.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("jwt: kid %q not found in JWKS", kid)
	}
	return key, nil
}

// jwksResponse is the top-level JWKS JSON structure.
type jwksResponse struct {
	Keys []map[string]any `json:"keys"`
}

func (f *jwksFetcher) fetch() error {
	// Try the shared cache first. On hit, decode and install keys without
	// an HTTP round trip. Cache errors (including ErrNotFound) fall
	// through to the live fetch.
	if f.cache != nil {
		if body, err := f.cache.Get(context.Background(), f.cacheKey()); err == nil && len(body) > 0 {
			if installErr := f.installKeysFromBody(body); installErr == nil {
				return nil
			}
			// Fall through: cached body unparseable; go live.
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("jwt: JWKS cache read failed for %s: %v", f.url, err)
		}
	}

	resp, err := f.client.Get(f.url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwt: JWKS endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return err
	}

	// Write-through the raw JSON to the cache for other instances.
	if f.cache != nil {
		ttl := f.refresh
		if ttl <= 0 {
			ttl = time.Hour
		}
		if serr := f.cache.Set(context.Background(), f.cacheKey(), body, ttl); serr != nil {
			log.Printf("jwt: JWKS cache write failed for %s: %v", f.url, serr)
		}
	}

	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("jwt: invalid JWKS JSON: %w", err)
	}

	newKeys := make(map[string]any, len(jwks.Keys))
	for _, key := range jwks.Keys {
		use, _ := key["use"].(string)
		if use != "" && use != "sig" {
			continue
		}
		kid, _ := key["kid"].(string)
		// Skip keys with empty kid when there are multiple keys (#13).
		if kid == "" && len(jwks.Keys) > 1 {
			continue
		}
		kty, _ := key["kty"].(string)
		switch kty {
		case "RSA":
			pub, err := parseRSAKey(key)
			if err != nil {
				continue
			}
			newKeys[kid] = pub
		case "EC":
			pub, err := parseECKey(key)
			if err != nil {
				continue
			}
			newKeys[kid] = pub
		case "OKP":
			pub, err := parseOKPKey(key)
			if err != nil {
				continue
			}
			newKeys[kid] = pub
		}
	}

	// Successful fetch replaces all keys to ensure revoked keys are purged.
	// Failed fetch preserves stale keys for availability.
	f.mu.Lock()
	f.keys = newKeys
	f.lastFetch = time.Now()
	f.mu.Unlock()

	return nil
}

// installKeysFromBody parses a raw JWKS JSON blob and installs it under
// f.mu. Split out from [fetch] so cache hits can reuse the parse path
// without duplicating RSA/EC/OKP decoding.
func (f *jwksFetcher) installKeysFromBody(body []byte) error {
	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("jwt: invalid JWKS JSON: %w", err)
	}
	newKeys := make(map[string]any, len(jwks.Keys))
	for _, key := range jwks.Keys {
		use, _ := key["use"].(string)
		if use != "" && use != "sig" {
			continue
		}
		kid, _ := key["kid"].(string)
		if kid == "" && len(jwks.Keys) > 1 {
			continue
		}
		kty, _ := key["kty"].(string)
		switch kty {
		case "RSA":
			if pub, err := parseRSAKey(key); err == nil {
				newKeys[kid] = pub
			}
		case "EC":
			if pub, err := parseECKey(key); err == nil {
				newKeys[kid] = pub
			}
		case "OKP":
			if pub, err := parseOKPKey(key); err == nil {
				newKeys[kid] = pub
			}
		}
	}
	f.mu.Lock()
	f.keys = newKeys
	f.lastFetch = time.Now()
	f.mu.Unlock()
	return nil
}

func parseRSAKey(key map[string]any) (*rsa.PublicKey, error) {
	nStr, _ := key["n"].(string)
	eStr, _ := key["e"].(string)
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("jwt: invalid RSA modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("jwt: invalid RSA exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, fmt.Errorf("jwt: RSA exponent too large")
	}

	pub := &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}
	// Enforce minimum 2048-bit RSA keys per NIST SP 800-57.
	if pub.N.BitLen() < 2048 {
		return nil, fmt.Errorf("jwt: RSA key size %d bits is below the 2048-bit minimum", pub.N.BitLen())
	}
	return pub, nil
}

func parseECKey(key map[string]any) (*ecdsa.PublicKey, error) {
	crv, _ := key["crv"].(string)
	xStr, _ := key["x"].(string)
	yStr, _ := key["y"].(string)
	xBytes, err := base64.RawURLEncoding.DecodeString(xStr)
	if err != nil {
		return nil, fmt.Errorf("jwt: invalid EC x coordinate: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(yStr)
	if err != nil {
		return nil, fmt.Errorf("jwt: invalid EC y coordinate: %w", err)
	}

	var ecdhCurve ecdh.Curve
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		ecdhCurve = ecdh.P256()
		curve = elliptic.P256()
	case "P-384":
		ecdhCurve = ecdh.P384()
		curve = elliptic.P384()
	case "P-521":
		ecdhCurve = ecdh.P521()
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("jwt: unsupported EC curve: %s", crv)
	}

	// Build the uncompressed point (0x04 || X || Y) and validate via ecdh.
	byteLen := (curve.Params().BitSize + 7) / 8
	uncompressed := make([]byte, 1+2*byteLen)
	uncompressed[0] = 0x04
	copy(uncompressed[1+byteLen-len(xBytes):1+byteLen], xBytes)
	copy(uncompressed[1+2*byteLen-len(yBytes):], yBytes)

	if _, err := ecdhCurve.NewPublicKey(uncompressed); err != nil {
		return nil, fmt.Errorf("jwt: EC point is not on curve %s", crv)
	}

	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)

	return &ecdsa.PublicKey{
		Curve: curve,
		X:     x,
		Y:     y,
	}, nil
}

func parseOKPKey(key map[string]any) (ed25519.PublicKey, error) {
	crv, _ := key["crv"].(string)
	if crv != "Ed25519" {
		return nil, fmt.Errorf("jwt: unsupported OKP curve: %s", crv)
	}
	xStr, _ := key["x"].(string)
	xBytes, err := base64.RawURLEncoding.DecodeString(xStr)
	if err != nil || len(xBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("jwt: invalid Ed25519 public key")
	}
	return ed25519.PublicKey(xBytes), nil
}
