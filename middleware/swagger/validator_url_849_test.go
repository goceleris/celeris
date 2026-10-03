package swagger

import (
	"strings"
	"testing"
)

// celeris#849: UIConfig.ValidatorURL opts back into a validator badge.

// TestValidatorURLOptIn849: a configured validator reaches SwaggerUIBundle
// as an escaped JS string, for every Swagger UI asset source; Scalar and
// ReDoc pages have no validator setting.
func TestValidatorURLOptIn849(t *testing.T) {
	t.Parallel()
	for _, v := range swaggerUIAssetsVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			for _, want := range []string{"https://validator.example/validator", hostile} {
				body := servePage(t, Config{
					SpecContent: jsonSpec,
					AssetsPath:  v.assetsPath,
					CDN:         v.cdn,
					UI:          UIConfig{ValidatorURL: want},
				})
				assertJSString(t, body, validatorURLRe849, want)
			}
		})
	}
	for _, r := range []UIRenderer{RendererScalar, RendererReDoc} {
		body := servePage(t, Config{SpecContent: jsonSpec, Renderer: r, CDN: true, UI: UIConfig{ValidatorURL: "https://validator.example/validator"}})
		if strings.Contains(body, "validator") {
			t.Errorf("%s page mentions a validator:\n%s", r, body)
		}
	}
}
