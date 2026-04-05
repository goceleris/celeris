// Package jwtparse implements a zero-allocation JWT parser and signer.
package jwtparse

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Claims is the interface for JWT claims validation.
type Claims interface {
	Valid() error
}

// NumericDate represents a JSON numeric date value (Unix timestamp).
type NumericDate struct {
	time.Time
}

// NewNumericDate creates a NumericDate from a time.Time.
func NewNumericDate(t time.Time) *NumericDate {
	return &NumericDate{Time: t.Truncate(time.Second)}
}

// MarshalJSON encodes a NumericDate as a Unix timestamp integer.
func (d NumericDate) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return []byte(fmt.Sprintf("%d", d.Unix())), nil
}

// UnmarshalJSON decodes a Unix timestamp number into a NumericDate.
func (d *NumericDate) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}

	f, err := n.Float64()
	if err != nil {
		return err
	}

	sec, dec := math.Modf(f)
	d.Time = time.Unix(int64(sec), int64(dec*1e9))
	return nil
}

// Audience is a slice of strings that accepts both a single string and an array in JSON.
type Audience []string

// UnmarshalJSON decodes an audience from either a single string or a string array.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = Audience{s}
		return nil
	}

	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*a = arr
	return nil
}

// RegisteredClaims are the IANA-registered JWT claims.
type RegisteredClaims struct {
	Issuer    string       `json:"iss,omitempty"`
	Subject   string       `json:"sub,omitempty"`
	Audience  Audience     `json:"aud,omitempty"`
	ExpiresAt *NumericDate `json:"exp,omitempty"`
	NotBefore *NumericDate `json:"nbf,omitempty"`
	IssuedAt  *NumericDate `json:"iat,omitempty"`
	ID        string       `json:"jti,omitempty"`
}

// Valid validates exp, nbf, and iat claims against the current time.
func (c RegisteredClaims) Valid() error {
	now := time.Now()

	if c.ExpiresAt != nil && now.After(c.ExpiresAt.Time) {
		return ErrTokenExpired
	}

	if c.NotBefore != nil && now.Before(c.NotBefore.Time) {
		return ErrTokenNotValidYet
	}

	if c.IssuedAt != nil && now.Before(c.IssuedAt.Time) {
		return ErrTokenUsedBeforeIssued
	}

	return nil
}

// MapClaims is an unstructured claims type backed by a map.
//
// Numeric claims (exp, nbf, iat) are stored as float64 when parsed from
// JSON by the internal parser. When constructing MapClaims
// programmatically, int64 and int values are also accepted by
// numericClaim and the time-validation methods. json.Number (from
// encoding/json with UseNumber) is also supported.
type MapClaims map[string]any

// Valid validates exp, nbf, and iat claims against the current time.
func (c MapClaims) Valid() error {
	now := time.Now().Unix()

	if exp, ok := c.numericClaim("exp"); ok {
		if now > exp {
			return ErrTokenExpired
		}
	}

	if nbf, ok := c.numericClaim("nbf"); ok {
		if now < nbf {
			return ErrTokenNotValidYet
		}
	}

	if iat, ok := c.numericClaim("iat"); ok {
		if now < iat {
			return ErrTokenUsedBeforeIssued
		}
	}

	return nil
}

// numericClaim extracts a numeric claim as int64. Handles float64 (from
// the internal parser), json.Number (from encoding/json), and int64/int
// (from programmatic MapClaims construction).
func (c MapClaims) numericClaim(key string) (int64, bool) {
	v, ok := c[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
