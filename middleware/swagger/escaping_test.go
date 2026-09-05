package swagger

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// hostile is a configuration value that is malformed for every embedding
// context the generated pages use: a double quote (terminates an HTML
// attribute or a JS string literal), a single quote, a closing script tag
// (terminates the inline <script> block regardless of quoting), an
// ampersand (HTML entity start), a BEL control character (Go's %q emits
// `\a`, which is not a JS/JSON escape) and U+2028 (a JS line terminator).
const hostile = "x\"y'z</script>&\a end"

// assetsVariants covers both Sprintf sites in every renderer: the CDN
// template and the AssetsPath template.
var assetsVariants = []struct {
	name       string
	assetsPath string
}{
	{"cdn", ""},
	{"assets", "/assets"},
}

// jsStringLiteral matches a double-quoted string literal (JSON grammar):
// either a non-quote non-backslash character or a backslash escape pair.
const jsStringLiteral = `("(?:[^"\\]|\\.)*")`

var (
	dataURLRe          = regexp.MustCompile(`data-url="([^"]*)"`)
	swaggerURLRe       = regexp.MustCompile(`\n  url: ` + jsStringLiteral + `,\n`)
	oauth2RedirectRe   = regexp.MustCompile(`oauth2RedirectUrl: ` + jsStringLiteral + `\n`)
	oauth2ClientIDRe   = regexp.MustCompile(`clientId: ` + jsStringLiteral + `[,}]`)
	oauth2RealmRe      = regexp.MustCompile(`realm: ` + jsStringLiteral + `[,}]`)
	oauth2AppNameRe    = regexp.MustCompile(`appName: ` + jsStringLiteral + `[,}]`)
	oauth2ScopesRe     = regexp.MustCompile(`scopes: ` + jsStringLiteral + `[,}]`)
	redocInitRe        = regexp.MustCompile(`Redoc\.init\(` + jsStringLiteral + `, `)
	inlineScriptBodyRe = regexp.MustCompile(`(?s)<script>\n(.*?)</script>`)
)

func servePage(t *testing.T, cfg Config) string {
	t.Helper()
	mw := New(cfg)
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/swagger/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	return rec.BodyString()
}

// assertJSString extracts the single JS string literal matched by re and
// asserts it is valid JSON whose decoded value round-trips to want.
func assertJSString(t *testing.T, body string, re *regexp.Regexp, want string) {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no JS string literal matched %s in:\n%s", re, body)
	}
	lit := m[1]
	if strings.Contains(lit, "</script>") {
		t.Fatalf("JS string literal %s contains a raw </script>", lit)
	}
	var got string
	if err := json.Unmarshal([]byte(lit), &got); err != nil {
		t.Fatalf("JS string literal %s is not valid JSON: %v", lit, err)
	}
	if got != want {
		t.Fatalf("JS string literal %s decodes to %q, want %q", lit, got, want)
	}
}

// assertScriptBlocksBalanced asserts that no interpolated value terminated
// an inline <script> block early: every <script opening has exactly one
// </script> closing, and the inline block still ends with the expected
// trailer.
func assertScriptBlocksBalanced(t *testing.T, body, trailer string) {
	t.Helper()
	if o, c := strings.Count(body, "<script"), strings.Count(body, "</script>"); o != c {
		t.Fatalf("unbalanced script tags: %d <script vs %d </script> in:\n%s", o, c, body)
	}
	m := inlineScriptBodyRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no inline <script> block in:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(m[1]), trailer) {
		t.Fatalf("inline script body was cut short; want trailer %q, got:\n%s", trailer, m[1])
	}
}

func TestSwaggerUIEscapesJSStringLiterals(t *testing.T) {
	t.Parallel()
	for _, v := range assetsVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			body := servePage(t, Config{
				SpecURL:    hostile,
				AssetsPath: v.assetsPath,
				UI: UIConfig{
					Title:             hostile,
					OAuth2RedirectURL: hostile,
					OAuth2: &OAuth2Config{
						ClientID: hostile,
						Realm:    hostile,
						AppName:  hostile,
						Scopes:   []string{hostile, "read"},
					},
				},
			})
			assertScriptBlocksBalanced(t, body, "});")
			assertJSString(t, body, swaggerURLRe, hostile)
			assertJSString(t, body, oauth2RedirectRe, hostile)
			assertJSString(t, body, oauth2ClientIDRe, hostile)
			assertJSString(t, body, oauth2RealmRe, hostile)
			assertJSString(t, body, oauth2AppNameRe, hostile)
			assertJSString(t, body, oauth2ScopesRe, hostile+" read")
			assertContains(t, body, "<title>"+html.EscapeString(hostile)+"</title>")
		})
	}
}

func TestScalarEscapesDataURLAttribute(t *testing.T) {
	t.Parallel()
	for _, v := range assetsVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			body := servePage(t, Config{
				SpecURL:    hostile,
				Renderer:   RendererScalar,
				AssetsPath: v.assetsPath,
				UI:         UIConfig{Title: hostile},
			})
			if o, c := strings.Count(body, "<script"), strings.Count(body, "</script>"); o != c {
				t.Fatalf("unbalanced script tags: %d <script vs %d </script> in:\n%s", o, c, body)
			}
			m := dataURLRe.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("no data-url attribute in:\n%s", body)
			}
			attr := m[1]
			for _, raw := range []string{`"`, `'`, `<`, `>`} {
				if strings.Contains(attr, raw) {
					t.Fatalf("data-url attribute value %q contains raw %s", attr, raw)
				}
			}
			if got := html.UnescapeString(attr); got != hostile {
				t.Fatalf("data-url attribute %q unescapes to %q, want %q", attr, got, hostile)
			}
			// The adjacent data-configuration attribute keeps its existing
			// html.EscapeString(json.Marshal(options)) escaping.
			assertContains(t, body, `data-configuration='`+html.EscapeString(`{"theme":"default"}`)+`'`)
			assertContains(t, body, "<title>"+html.EscapeString(hostile)+"</title>")
		})
	}
}

func TestReDocEscapesJSStringLiteral(t *testing.T) {
	t.Parallel()
	for _, v := range assetsVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			body := servePage(t, Config{
				SpecURL:    hostile,
				Renderer:   RendererReDoc,
				AssetsPath: v.assetsPath,
				UI:         UIConfig{Title: hostile},
			})
			assertScriptBlocksBalanced(t, body, `document.getElementById("redoc-container"));`)
			assertJSString(t, body, redocInitRe, hostile)
			assertContains(t, body, "<title>"+html.EscapeString(hostile)+"</title>")
		})
	}
}

// TestPlainConfigUnchanged pins the served values for ordinary inputs: the
// context-correct escapers must render a plain SpecURL and plain OAuth2
// values exactly as before.
func TestPlainConfigUnchanged(t *testing.T) {
	t.Parallel()
	for _, v := range assetsVariants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			oauth := &OAuth2Config{
				ClientID: "my-client",
				Realm:    "my-realm",
				AppName:  "My App",
				Scopes:   []string{"read", "write"},
				UsePKCE:  true,
			}

			body := servePage(t, Config{
				SpecContent: jsonSpec,
				AssetsPath:  v.assetsPath,
				UI: UIConfig{
					OAuth2RedirectURL: "https://example.com/oauth2-redirect",
					OAuth2:            oauth,
				},
			})
			assertContains(t, body, "\n  url: \"/swagger/spec\",\n")
			assertContains(t, body, `docExpansion: "list",`)
			assertContains(t, body, `oauth2RedirectUrl: "https://example.com/oauth2-redirect"`)
			assertContains(t, body, `ui.initOAuth({clientId: "my-client", usePkceWithAuthorizationCodeGrant: true, realm: "my-realm", appName: "My App", scopes: "read write"});`)

			body = servePage(t, Config{
				SpecURL:    "https://example.com/openapi.json",
				Renderer:   RendererScalar,
				AssetsPath: v.assetsPath,
			})
			assertContains(t, body, `<script id="api-reference" data-url="https://example.com/openapi.json" data-configuration='`+html.EscapeString(`{"theme":"default"}`)+`'></script>`)

			body = servePage(t, Config{
				SpecContent: jsonSpec,
				Renderer:    RendererReDoc,
				AssetsPath:  v.assetsPath,
			})
			assertContains(t, body, `Redoc.init("/swagger/spec", {}, document.getElementById("redoc-container"));`)
		})
	}
}
