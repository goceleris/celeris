package swagger

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

// celeris#888 item 75: the Scalar page asked fonts.scalar.com for its fonts by
// default (Scalar's withDefaultFonts option is on unless the page turns it
// off), a third-party request that sends the viewer's address and the page's
// Referer to a host the application did not name. A page built with no
// Options now sets withDefaultFonts to false, and so does one built with
// Options that do not mention it; Options {"withDefaultFonts": true} opts back
// in. Scalar's request proxy (proxy.scalar.com) is used only when proxyUrl is
// set, which this package never does (checked in a browser against
// @scalar/api-reference 1.72.4: a Try-it request goes straight to the spec's
// server), so no change is needed for it.

const page75 = "http://app.test/swagger/"

var urlInText75 = regexp.MustCompile(`https?://[^\s"'<>\\)]+`)

// externalHosts75 returns the hosts, other than the page's own, that the HTML
// of a page names in a URL attribute (src, href, data-url, action, ...), in a
// stylesheet @import or url(), or in the JSON of a data-configuration
// attribute, in sorted order.
func externalHosts75(t *testing.T, body string) []string {
	t.Helper()
	base, err := url.Parse(page75)
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.HasPrefix(ref, "#") {
			return
		}
		u, err := url.Parse(ref)
		if err != nil {
			t.Errorf("unparseable reference %q: %v", ref, err)
			return
		}
		if h := base.ResolveReference(u).Host; h != "" && h != base.Host {
			hosts[h] = true
		}
	}
	doc, err := xhtml.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for _, a := range n.Attr {
				switch a.Key {
				case "src", "href", "data-url", "action", "poster", "data", "srcset", "ping":
					add(a.Val)
				}
				// Whatever an attribute or inline text names as an absolute URL.
				for _, m := range urlInText75.FindAllString(a.Val, -1) {
					add(m)
				}
			}
		}
		if n.Type == xhtml.TextNode {
			for _, m := range urlInText75.FindAllString(n.Data, -1) {
				add(m)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	out := make([]string, 0, len(hosts))
	for h := range hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// scalarConfig75 returns the page's data-configuration JSON, decoded.
func scalarConfig75(t *testing.T, body string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`data-configuration='([^']*)'`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-configuration attribute in:\n%s", body)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &cfg); err != nil {
		t.Fatalf("data-configuration %q is not JSON: %v", m[1], err)
	}
	return cfg
}

// With the files served by the application (AssetsPath), the default page of
// every renderer names no host but its own.
func TestDefaultPagesNameNoThirdPartyHost75(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"swagger-ui-embedded", Config{SpecContent: jsonSpec}},
		{"scalar-assets-path", Config{SpecContent: jsonSpec, Renderer: RendererScalar, AssetsPath: "/assets"}},
		{"redoc-assets-path", Config{SpecContent: jsonSpec, Renderer: RendererReDoc, AssetsPath: "/assets"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if hosts := externalHosts75(t, servePage(t, tc.cfg)); len(hosts) != 0 {
				t.Errorf("the default page names third-party hosts %v", hosts)
			}
		})
	}
}

// CDN: true names jsDelivr for the renderer's script (it is the opt-in for
// exactly that) and nothing else.
func TestCDNPagesNameOnlyTheCDN75(t *testing.T) {
	t.Parallel()
	for _, r := range []UIRenderer{RendererSwaggerUI, RendererScalar, RendererReDoc} {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			got := externalHosts75(t, servePage(t, Config{SpecContent: jsonSpec, Renderer: r, CDN: true}))
			if len(got) != 1 || got[0] != "cdn.jsdelivr.net" {
				t.Errorf("the CDN page names hosts %v, want only cdn.jsdelivr.net", got)
			}
		})
	}
}

// The Scalar page turns Scalar's default fonts off unless the Options say
// otherwise, and the host's Options map is not modified to do it.
func TestScalarDefaultFontsOffUnlessOptedIn75(t *testing.T) {
	t.Parallel()
	const key = "withDefaultFonts"
	for _, tc := range []struct {
		name    string
		options map[string]any
		want    bool
		theme   any // the Options' own theme, if any
	}{
		{"nil-options", nil, false, "default"},
		{"empty-options", map[string]any{}, false, nil},
		{"options-without-the-key", map[string]any{"theme": "purple"}, false, "purple"},
		{"opt-in", map[string]any{key: true, "theme": "purple"}, true, "purple"},
		{"explicit-false", map[string]any{key: false}, false, nil},
	} {
		for _, cdn := range []bool{false, true} {
			name := tc.name + "/assets-path"
			cfg := Config{SpecContent: jsonSpec, Renderer: RendererScalar, AssetsPath: "/assets", Options: tc.options}
			if cdn {
				name = tc.name + "/cdn"
				cfg = Config{SpecContent: jsonSpec, Renderer: RendererScalar, CDN: true, Options: tc.options}
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var before map[string]any
				if tc.options != nil {
					before = make(map[string]any, len(tc.options))
					for k, v := range tc.options {
						before[k] = v
					}
				}
				got := scalarConfig75(t, servePage(t, cfg))
				if v, ok := got[key]; !ok || v != tc.want {
					t.Errorf("data-configuration %s = %v (present %v), want %v", key, v, ok, tc.want)
				}
				if tc.theme != nil && got["theme"] != tc.theme {
					t.Errorf("data-configuration theme = %v, want %v", got["theme"], tc.theme)
				}
				if tc.options != nil && len(tc.options) != len(before) {
					t.Errorf("the host's Options map was modified: %v, was %v", tc.options, before)
				}
				if _, set := got["proxyUrl"]; set {
					t.Errorf("the page sets proxyUrl: %v", got["proxyUrl"])
				}
			})
		}
	}
}

// ReDoc does not read withDefaultFonts: its page keeps the Options as given.
func TestReDocOptionsAreNotTouched75(t *testing.T) {
	t.Parallel()
	body := servePage(t, Config{SpecContent: jsonSpec, Renderer: RendererReDoc, CDN: true})
	if strings.Contains(body, "withDefaultFonts") {
		t.Errorf("the ReDoc page carries Scalar's option:\n%s", body)
	}
}
