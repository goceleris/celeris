package keyauth

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris"
)

// The keyauth sentinels must satisfy errors.Is(err, celeris.ErrUnauthorized)
// so a mixed auth stack (jwt + keyauth + basicauth) can match every 401 with a
// single check, as documented on celeris.ErrUnauthorized and keyauth/doc.go.
//
// Regression guard for #391: ErrMissingKey used to wrap a plain errors.New, so
// its errors.Is chain ended before celeris.ErrUnauthorized and a centralized
// handler silently missed a missing API key — the most common keyauth failure.
func TestSentinelsMatchErrUnauthorized(t *testing.T) {
	for _, e := range []error{ErrMissingKey, ErrUnauthorized} {
		if !errors.Is(e, celeris.ErrUnauthorized) {
			t.Errorf("errors.Is(%v, celeris.ErrUnauthorized) = false, want true", e)
		}
	}
}

// Chaining the cause to ErrUnauthorized must not change the surfaced error
// text (HTTPError.Error stays byte-identical to the pre-fix message).
func TestErrMissingKeyMessageUnchanged(t *testing.T) {
	const want = "code=401, message=Unauthorized, err=keyauth: missing API key"
	if got := ErrMissingKey.Error(); got != want {
		t.Errorf("ErrMissingKey.Error() = %q, want %q", got, want)
	}
}
