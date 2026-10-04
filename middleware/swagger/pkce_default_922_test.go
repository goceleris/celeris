package swagger

import (
	"strings"
	"testing"
)

// TestOAuth2PKCEOnByDefault922 is celeris#922's family in swagger.
// OAuth2Config.UsePKCE was documented as "Default: true", but a bool field
// cannot tell "not set" from false: an OAuth2Config literal that left it out
// (OAuth2Config{ClientID: "c"}) turned PKCE off, in the browser flow that
// MUST use it. PKCE is now on unless DisablePKCE is set.
func TestOAuth2PKCEOnByDefault922(t *testing.T) {
	const pkce = `usePkceWithAuthorizationCodeGrant: true`
	body := servePage(t, Config{SpecContent: jsonSpec, UI: UIConfig{OAuth2: &OAuth2Config{ClientID: "c"}}})
	if !strings.Contains(body, pkce) {
		t.Errorf("OAuth2Config{ClientID: \"c\"}: the page does not enable PKCE (%s)", pkce)
	}
	body = servePage(t, Config{SpecContent: jsonSpec, UI: UIConfig{OAuth2: &OAuth2Config{ClientID: "c", DisablePKCE: true}}})
	if strings.Contains(body, "usePkceWithAuthorizationCodeGrant") {
		t.Errorf("OAuth2Config{DisablePKCE: true}: the page still enables PKCE")
	}
	if !strings.Contains(body, `clientId: "c"`) {
		t.Errorf("OAuth2Config{DisablePKCE: true}: the page lost the OAuth2 settings")
	}
}
