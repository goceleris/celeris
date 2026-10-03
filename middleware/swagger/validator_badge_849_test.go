package swagger

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// celeris#849: with the default config the Swagger UI page must not send
// its spec URL to an online validator. Swagger UI's badge is on unless the
// page sets validatorUrl to a value requiresValidationURL rejects.

var validatorURLRe849 = regexp.MustCompile(`\n  validatorUrl: ` + jsStringLiteral + `[,\n]`)

// pageValidatorURL849 returns the validatorUrl the page passes to
// SwaggerUIBundle, failing the test when the page sets none (Swagger UI
// then uses https://validator.swagger.io/validator).
func pageValidatorURL849(t *testing.T, body string) string {
	t.Helper()
	m := validatorURLRe849.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page passes no validatorUrl to SwaggerUIBundle, so Swagger UI's badge loads https://validator.swagger.io/validator?url=<spec URL>:\n%s", body)
	}
	var got string
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		t.Fatalf("validatorUrl %s is not a JS string literal: %v", m[1], err)
	}
	return got
}

// TestValidatorBadgeOffByDefault849: every Swagger UI page the defaults can
// produce (embedded, CDN and AssetsPath files; default, nested and root
// BasePath; with and without OAuth2 settings) turns the badge off.
func TestValidatorBadgeOffByDefault849(t *testing.T) {
	t.Parallel()
	for _, v := range swaggerUIAssetsVariants {
		for _, bp := range []string{"", "/docs/api", "/"} {
			for _, oauth := range []bool{false, true} {
				name := v.name + "/basepath=" + bp
				if oauth {
					name += "/oauth2"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					cfg := Config{SpecContent: jsonSpec, BasePath: bp, AssetsPath: v.assetsPath, CDN: v.cdn}
					if oauth {
						cfg.UI.OAuth2RedirectURL = "https://app.test/swagger/oauth2-redirect.html"
						cfg.UI.OAuth2 = &OAuth2Config{ClientID: "c", UsePKCE: true}
					}
					uiPath := strings.TrimRight(bp, "/") + "/"
					if bp == "" {
						uiPath = "/swagger/"
					}
					if got := pageValidatorURL849(t, servePageAt425(t, cfg, uiPath)); got != "none" {
						t.Fatalf("validatorUrl = %q, want \"none\" (the badge off)", got)
					}
				})
			}
		}
	}
}

// TestPinnedBundleBadgeSemantics849 checks, in the embedded swagger-ui-dist
// bundles, the code the default relies on: the standalone layout renders
// the badge, an unset validatorUrl means validator.swagger.io, and "none"
// turns the badge off. A version bump that changes any of it fails here.
func TestPinnedBundleBadgeSemantics849(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		file, code, why string
		body            []byte
	}{
		{"swagger-ui-standalone-preset.js", `s=t("onlineValidatorBadge",!0)`, "StandaloneLayout renders the badge", swaggerUIPreset},
		{"swagger-ui-bundle.js", `validatorUrl:void 0===a?"https://validator.swagger.io/validator":a`, "an unset validatorUrl is validator.swagger.io", swaggerUIBundle},
		{"swagger-ui-bundle.js", `function requiresValidationURL(s){return!(!s||s.indexOf("localhost")>=0||s.indexOf("127.0.0.1")>=0||"none"===s)}`, `"none" turns the badge off`, swaggerUIBundle},
		{"swagger-ui-bundle.js", `this.state.url&&requiresValidationURL(this.state.validatorUrl)&&requiresValidationURL(this.state.url)`, "the badge renders only when requiresValidationURL accepts the validator", swaggerUIBundle},
	} {
		if n := strings.Count(string(c.body), c.code); n != 1 {
			t.Errorf("%s: %q occurs %d times, want 1 (%s)", c.file, c.code, n, c.why)
		}
	}
}
