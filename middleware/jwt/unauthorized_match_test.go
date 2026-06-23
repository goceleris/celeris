package jwt

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris"
)

// The jwt sentinels must satisfy errors.Is(err, celeris.ErrUnauthorized) so a
// mixed auth stack (jwt + keyauth + basicauth) can match every 401 with one
// check, as documented on celeris.ErrUnauthorized.
func TestSentinelsMatchErrUnauthorized(t *testing.T) {
	for _, e := range []error{ErrTokenMissing, ErrTokenInvalid, ErrJWTExpired, ErrJWTMalformed} {
		if !errors.Is(e, celeris.ErrUnauthorized) {
			t.Errorf("errors.Is(%v, celeris.ErrUnauthorized) = false, want true", e)
		}
		// The classifiedError wrapper (used on the real reject paths) must match too.
		ce := &classifiedError{outer: e, inner: errors.New("detail")}
		if !errors.Is(ce, celeris.ErrUnauthorized) {
			t.Errorf("errors.Is(classifiedError{%v}, celeris.ErrUnauthorized) = false, want true", e)
		}
	}
}

// Wrapping ErrUnauthorized must not change the surfaced error text.
func TestSentinelMessagesUnchanged(t *testing.T) {
	cases := map[*celeris.HTTPError]string{
		ErrTokenMissing: "code=401, message=Unauthorized, err=jwt: missing or malformed token",
		ErrTokenInvalid: "code=401, message=Unauthorized, err=jwt: invalid or expired token",
		ErrJWTExpired:   "code=401, message=Unauthorized, err=jwt: token has expired",
		ErrJWTMalformed: "code=401, message=Unauthorized, err=jwt: token is malformed",
	}
	for e, want := range cases {
		if got := e.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
}
