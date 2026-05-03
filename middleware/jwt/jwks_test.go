package jwt

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/jwt/internal/jwtparse"
)

func rsaJWKSJSON(kid string, pub *rsa.PublicKey) []byte {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": kid,
				"use": "sig",
				"n":   n,
				"e":   e,
				"alg": "RS256",
			},
		},
	}
	b, _ := json.Marshal(jwks)
	return b
}

func ecJWKSJSON(kid string, pub *ecdsa.PublicKey, crv string) []byte {
	ecdhKey, err := pub.ECDH()
	if err != nil {
		panic(err)
	}
	raw := ecdhKey.Bytes() // 0x04 || X || Y
	byteLen := (len(raw) - 1) / 2
	x := base64.RawURLEncoding.EncodeToString(raw[1 : 1+byteLen])
	y := base64.RawURLEncoding.EncodeToString(raw[1+byteLen:])
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "EC",
				"kid": kid,
				"use": "sig",
				"crv": crv,
				"x":   x,
				"y":   y,
				"alg": "ES256",
			},
		},
	}
	b, _ := json.Marshal(jwks)
	return b
}

func TestJWKSFetchRSA(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("rsa-kid-1", &privKey.PublicKey)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	if err := fetcher.fetch(); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	fetcher.mu.RLock()
	key, ok := fetcher.keys["rsa-kid-1"]
	fetcher.mu.RUnlock()
	if !ok {
		t.Fatal("rsa-kid-1 not found in keys")
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", key)
	}
	if pub.N.Cmp(privKey.N) != 0 {
		t.Fatal("RSA modulus mismatch")
	}
}

func TestJWKSFetchEC(t *testing.T) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := ecJWKSJSON("ec-kid-1", &privKey.PublicKey, "P-256")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	if err := fetcher.fetch(); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	fetcher.mu.RLock()
	key, ok := fetcher.keys["ec-kid-1"]
	fetcher.mu.RUnlock()
	if !ok {
		t.Fatal("ec-kid-1 not found in keys")
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("expected *ecdsa.PublicKey, got %T", key)
	}
	if !pub.Equal(&privKey.PublicKey) {
		t.Fatal("EC key mismatch")
	}
}

func TestJWKSFetchECCurves(t *testing.T) {
	curves := []struct {
		name  string
		curve elliptic.Curve
	}{
		{"P-256", elliptic.P256()},
		{"P-384", elliptic.P384()},
		{"P-521", elliptic.P521()},
	}
	for _, tc := range curves {
		t.Run(tc.name, func(t *testing.T) {
			privKey, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			body := ecJWKSJSON("ec-"+tc.name, &privKey.PublicKey, tc.name)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer ts.Close()

			fetcher := newJWKSFetcher(ts.URL, time.Hour)
			if err := fetcher.fetch(); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			fetcher.mu.RLock()
			key, ok := fetcher.keys["ec-"+tc.name]
			fetcher.mu.RUnlock()
			if !ok {
				t.Fatalf("ec-%s not found", tc.name)
			}
			pub, ok := key.(*ecdsa.PublicKey)
			if !ok {
				t.Fatalf("expected *ecdsa.PublicKey, got %T", key)
			}
			if pub.Curve != tc.curve {
				t.Fatalf("curve mismatch: got %v, want %v", pub.Curve.Params().Name, tc.name)
			}
		})
	}
}

func TestJWKSRefreshStaleKeys(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	kid1Body := rsaJWKSJSON("kid-v1", &privKey.PublicKey)
	kid2Body := rsaJWKSJSON("kid-v2", &privKey.PublicKey)

	var callCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_, _ = w.Write(kid1Body)
		} else {
			_, _ = w.Write(kid2Body)
		}
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, 10*time.Millisecond)

	// Initial fetch.
	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "kid-v1"}}
	key, err := fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("first keyFunc: %v", err)
	}
	if _, ok := key.(*rsa.PublicKey); !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", key)
	}

	// Wait for keys to go stale.
	time.Sleep(20 * time.Millisecond)

	// Request kid-v2, which should trigger a refresh.
	tok2 := &jwtparse.Token{Header: jwtparse.Header{Kid: "kid-v2"}}
	key2, err := fetcher.keyFunc(tok2)
	if err != nil {
		t.Fatalf("second keyFunc: %v", err)
	}
	if _, ok := key2.(*rsa.PublicKey); !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", key2)
	}

	if callCount.Load() < 2 {
		t.Fatal("expected at least 2 fetches")
	}
}

func TestJWKSUnknownKidAfterRefresh(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("known-kid", &privKey.PublicKey)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)

	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "nonexistent-kid"}}
	_, err = fetcher.keyFunc(tok)
	if err == nil {
		t.Fatal("expected error for unknown kid")
	}
	if !contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestJWKSMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	err := fetcher.fetch()
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !contains(err.Error(), "invalid JWKS JSON") {
		t.Fatalf("expected 'invalid JWKS JSON' error, got: %v", err)
	}
}

func TestJWKSNon200Response(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	err := fetcher.fetch()
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !contains(err.Error(), "503") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}

func TestJWKSResponseSizeLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write a valid JSON start, then pad with spaces to exceed 1MB limit,
		// then close JSON. The LimitReader caps at 1MB so the JSON will be truncated.
		_, _ = w.Write([]byte(`{"keys":[`))
		buf := make([]byte, 1<<20) // 1MB of padding
		for i := range buf {
			buf[i] = ' '
		}
		_, _ = w.Write(buf)
		_, _ = w.Write([]byte(`]}`))
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	err := fetcher.fetch()
	if err == nil {
		t.Fatal("expected error for oversized response")
	}
}

func TestJWKSHTTPTimeout(t *testing.T) {
	// Asserts the client times out after 50ms while the server takes
	// 15s. The unblocking `stop` signal lets ts.Close return promptly,
	// avoiding httptest's "Server blocked in Close" warning that
	// otherwise flakes -count=2+ runs.
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-stop:
		case <-time.After(15 * time.Second):
		}
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer func() {
		close(stop)
		ts.Close()
	}()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	// Override client timeout to 50ms for fast test.
	fetcher.client.Timeout = 50 * time.Millisecond

	err := fetcher.fetch()
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestJWKSHTTPSEnforcement(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantPanic bool
	}{
		{"https allowed", "https://example.com/jwks", false},
		{"http external rejected", "http://external.example.com/jwks", true},
		{"http localhost allowed", "http://localhost:8080/jwks", false},
		{"http 127.0.0.1 allowed", "http://127.0.0.1:8080/jwks", false},
		{"http ::1 allowed", "http://[::1]:8080/jwks", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var panicked bool
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
					}
				}()
				validateJWKSURL(tc.url)
			}()
			if panicked != tc.wantPanic {
				t.Fatalf("panicked=%v, want %v", panicked, tc.wantPanic)
			}
		})
	}
}

func TestJWKSThunderingHerd(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("herd-kid", &privKey.PublicKey)

	var fetchCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCount.Add(1)
		// Simulate slow fetch.
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, 10*time.Millisecond)

	// Do one initial fetch to populate keys.
	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "herd-kid"}}
	_, err = fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("initial fetch: %v", err)
	}

	initialCount := fetchCount.Load()

	// Wait for keys to go stale.
	time.Sleep(20 * time.Millisecond)

	// Fire many concurrent requests that all find stale keys.
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "herd-kid"}}
			_, errs[idx] = fetcher.keyFunc(tok)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d: %v", i, e)
		}
	}

	// Only one additional fetch should have occurred.
	additionalFetches := fetchCount.Load() - initialCount
	if additionalFetches > 1 {
		t.Fatalf("expected at most 1 additional fetch, got %d", additionalFetches)
	}
}

func TestJWKSKeyFuncIntegration(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("int-kid", &privKey.PublicKey)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	// Create a token signed with the RSA key.
	claims := jwtparse.MapClaims{"sub": "jwks-user"}
	tokenStr := signTokenWithKidAndMethod(jwtparse.SigningMethodRS256, claims, "int-kid", privKey)

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{"RS256"}))

	parsed, err := parser.ParseWithClaims(tokenStr, jwtparse.MapClaims{}, fetcher.keyFunc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	mc := parsed.Claims.(jwtparse.MapClaims)
	if mc["sub"] != "jwks-user" {
		t.Fatalf("sub: got %v, want jwks-user", mc["sub"])
	}
}

func TestJWKSKeyFuncECIntegration(t *testing.T) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := ecJWKSJSON("ec-int-kid", &privKey.PublicKey, "P-256")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	claims := jwtparse.MapClaims{"sub": "ec-user"}
	tokenStr := signTokenWithKidAndMethod(jwtparse.SigningMethodES256, claims, "ec-int-kid", privKey)

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{"ES256"}))

	parsed, err := parser.ParseWithClaims(tokenStr, jwtparse.MapClaims{}, fetcher.keyFunc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	mc := parsed.Claims.(jwtparse.MapClaims)
	if mc["sub"] != "ec-user" {
		t.Fatalf("sub: got %v, want ec-user", mc["sub"])
	}
}

func okpJWKSJSON(kid string, pub ed25519.PublicKey) []byte {
	x := base64.RawURLEncoding.EncodeToString(pub)
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "OKP",
				"kid": kid,
				"use": "sig",
				"crv": "Ed25519",
				"x":   x,
			},
		},
	}
	b, _ := json.Marshal(jwks)
	return b
}

func TestJWKSFetchOKPEdDSA(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := okpJWKSJSON("ed-kid-1", pub)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	if err := fetcher.fetch(); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	fetcher.mu.RLock()
	key, ok := fetcher.keys["ed-kid-1"]
	fetcher.mu.RUnlock()
	if !ok {
		t.Fatal("ed-kid-1 not found in keys")
	}
	edKey, ok := key.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("expected ed25519.PublicKey, got %T", key)
	}
	if len(edKey) != ed25519.PublicKeySize {
		t.Fatalf("key size: got %d, want %d", len(edKey), ed25519.PublicKeySize)
	}
}

func TestJWKSKeyFuncOKPIntegration(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := okpJWKSJSON("ed-int-kid", pub)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	claims := jwtparse.MapClaims{"sub": "ed-user"}
	tokenStr := signTokenWithKidAndMethod(jwtparse.SigningMethodEdDSA, claims, "ed-int-kid", priv)

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{"EdDSA"}))

	parsed, err := parser.ParseWithClaims(tokenStr, jwtparse.MapClaims{}, fetcher.keyFunc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	mc := parsed.Claims.(jwtparse.MapClaims)
	if mc["sub"] != "ed-user" {
		t.Fatalf("sub: got %v, want ed-user", mc["sub"])
	}
}

func TestJWKSOKPUnsupportedCurve(t *testing.T) {
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "OKP",
				"kid": "bad-curve",
				"use": "sig",
				"crv": "X25519",
				"x":   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			},
		},
	}
	body, _ := json.Marshal(jwks)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	if err := fetcher.fetch(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	fetcher.mu.RLock()
	_, ok := fetcher.keys["bad-curve"]
	fetcher.mu.RUnlock()
	if ok {
		t.Fatal("unsupported OKP curve should not be stored")
	}
}

func TestJWKSECCurveValidation(t *testing.T) {
	// Construct a JWKS with a point NOT on the P-256 curve.
	// Use (1, 1) which is not on P-256.
	x := base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes())
	y := base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes())
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "EC",
				"kid": "bad-point",
				"use": "sig",
				"crv": "P-256",
				"x":   x,
				"y":   y,
			},
		},
	}
	body, _ := json.Marshal(jwks)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	if err := fetcher.fetch(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	fetcher.mu.RLock()
	_, ok := fetcher.keys["bad-point"]
	fetcher.mu.RUnlock()
	if ok {
		t.Fatal("point not on curve should not be stored")
	}
}

func TestJWKSFetchAndCache(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("cache-kid", &privKey.PublicKey)

	var requestCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)

	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "cache-kid"}}

	// First call triggers a fetch.
	key, err := fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("first keyFunc: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
	if requestCount.Load() != 1 {
		t.Fatalf("expected 1 HTTP request after first call, got %d", requestCount.Load())
	}

	// Second call should use the cache (no additional HTTP request).
	key2, err := fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("second keyFunc: %v", err)
	}
	if key2 == nil {
		t.Fatal("expected non-nil cached key")
	}
	if requestCount.Load() != 1 {
		t.Fatalf("expected 1 HTTP request after second call (cached), got %d", requestCount.Load())
	}
}

func TestJWKSKeyRevocation(t *testing.T) {
	keyA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	bodyA := rsaJWKSJSON("kid-A", &keyA.PublicKey)
	bodyB := rsaJWKSJSON("kid-B", &keyB.PublicKey)

	var serveB atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serveB.Load() {
			_, _ = w.Write(bodyB)
		} else {
			_, _ = w.Write(bodyA)
		}
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, 10*time.Millisecond)

	// Fetch key A.
	tokA := &jwtparse.Token{Header: jwtparse.Header{Kid: "kid-A"}}
	key, err := fetcher.keyFunc(tokA)
	if err != nil {
		t.Fatalf("fetch key A: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key A")
	}

	// Switch server to serve only key B and force staleness.
	serveB.Store(true)
	fetcher.mu.Lock()
	fetcher.lastFetch = time.Now().Add(-time.Hour)
	fetcher.mu.Unlock()

	// Request key B, triggering a refresh.
	tokB := &jwtparse.Token{Header: jwtparse.Header{Kid: "kid-B"}}
	key, err = fetcher.keyFunc(tokB)
	if err != nil {
		t.Fatalf("fetch key B after rotation: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key B")
	}

	// Verify key A is gone (revoked key purged).
	fetcher.mu.RLock()
	_, hasA := fetcher.keys["kid-A"]
	_, hasB := fetcher.keys["kid-B"]
	fetcher.mu.RUnlock()
	if hasA {
		t.Fatal("revoked key A should have been purged after successful refresh")
	}
	if !hasB {
		t.Fatal("key B should be present after refresh")
	}
}

func TestJWKSParseRSAKey(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := &privKey.PublicKey

	jwk := map[string]any{
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}

	parsed, err := parseRSAKey(jwk)
	if err != nil {
		t.Fatalf("parseRSAKey: %v", err)
	}
	if parsed.N.Cmp(pub.N) != 0 {
		t.Fatal("RSA modulus mismatch")
	}
	if parsed.E != pub.E {
		t.Fatalf("RSA exponent: got %d, want %d", parsed.E, pub.E)
	}
}

func TestJWKSParseECKey(t *testing.T) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := &privKey.PublicKey

	ecdhKey, err := pub.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	raw := ecdhKey.Bytes() // 0x04 || X || Y
	byteLen := (len(raw) - 1) / 2

	jwk := map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(raw[1 : 1+byteLen]),
		"y":   base64.RawURLEncoding.EncodeToString(raw[1+byteLen:]),
	}

	parsed, err := parseECKey(jwk)
	if err != nil {
		t.Fatalf("parseECKey: %v", err)
	}
	if !parsed.Equal(pub) {
		t.Fatal("EC key mismatch")
	}
	if parsed.Curve != elliptic.P256() {
		t.Fatal("EC curve mismatch")
	}
}

func TestJWKSParseOKPKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	jwk := map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
	}

	parsed, err := parseOKPKey(jwk)
	if err != nil {
		t.Fatalf("parseOKPKey: %v", err)
	}
	if !parsed.Equal(pub) {
		t.Fatal("Ed25519 key mismatch")
	}
}

func TestJWKSFetchError(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := rsaJWKSJSON("error-kid", &privKey.PublicKey)

	var shouldFail atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if shouldFail.Load() {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, 10*time.Millisecond)

	// Initial successful fetch.
	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "error-kid"}}
	key, err := fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}

	// Make server fail, force staleness.
	shouldFail.Store(true)
	fetcher.mu.Lock()
	fetcher.lastFetch = time.Now().Add(-time.Hour)
	fetcher.mu.Unlock()

	// Stale keys should be preserved despite fetch failure.
	key2, err := fetcher.keyFunc(tok)
	if err != nil {
		t.Fatalf("stale fallback should succeed: %v", err)
	}
	if key2 == nil {
		t.Fatal("expected non-nil stale key")
	}
}

// --- Multi-JWKS (JWKSURLs) integration tests ---

func TestMultiJWKSKeyFuncFirstProviderHasKey(t *testing.T) {
	privKey1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privKey2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	body1 := rsaJWKSJSON("provider1-kid", &privKey1.PublicKey)
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body1)
	}))
	defer ts1.Close()

	body2 := rsaJWKSJSON("provider2-kid", &privKey2.PublicKey)
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body2)
	}))
	defer ts2.Close()

	fetcher1 := newJWKSFetcher(ts1.URL, time.Hour)
	fetcher2 := newJWKSFetcher(ts2.URL, time.Hour)
	kf := multiJWKSKeyFunc([]*jwksFetcher{fetcher1, fetcher2})

	// Token from provider 1.
	claims := jwtparse.MapClaims{"sub": "multi-user-1"}
	tokenStr := signTokenWithKidAndMethod(jwtparse.SigningMethodRS256, claims, "provider1-kid", privKey1)
	parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{"RS256"}))
	parsed, err := parser.ParseWithClaims(tokenStr, jwtparse.MapClaims{}, kf)
	if err != nil {
		t.Fatalf("parse provider1 token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestMultiJWKSKeyFuncSecondProviderHasKey(t *testing.T) {
	privKey1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privKey2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	body1 := rsaJWKSJSON("provider1-kid", &privKey1.PublicKey)
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body1)
	}))
	defer ts1.Close()

	body2 := rsaJWKSJSON("provider2-kid", &privKey2.PublicKey)
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body2)
	}))
	defer ts2.Close()

	fetcher1 := newJWKSFetcher(ts1.URL, time.Hour)
	fetcher2 := newJWKSFetcher(ts2.URL, time.Hour)
	kf := multiJWKSKeyFunc([]*jwksFetcher{fetcher1, fetcher2})

	// Token from provider 2 (kid not in provider 1).
	claims := jwtparse.MapClaims{"sub": "multi-user-2"}
	tokenStr := signTokenWithKidAndMethod(jwtparse.SigningMethodRS256, claims, "provider2-kid", privKey2)
	parser := jwtparse.NewParser(jwtparse.WithValidMethods([]string{"RS256"}))
	parsed, err := parser.ParseWithClaims(tokenStr, jwtparse.MapClaims{}, kf)
	if err != nil {
		t.Fatalf("parse provider2 token: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
}

func TestMultiJWKSKeyFuncUnknownKidAllProviders(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	body := rsaJWKSJSON("known-kid", &privKey.PublicKey)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer ts.Close()

	fetcher := newJWKSFetcher(ts.URL, time.Hour)
	kf := multiJWKSKeyFunc([]*jwksFetcher{fetcher})

	tok := &jwtparse.Token{Header: jwtparse.Header{Kid: "unknown-kid"}}
	_, err = kf(tok)
	if err == nil {
		t.Fatal("expected error for unknown kid across all providers")
	}
}
