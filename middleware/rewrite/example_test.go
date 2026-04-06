package rewrite_test

import (
	"github.com/goceleris/celeris/middleware/rewrite"
)

func ExampleNew() {
	// Silent rewrite: /old is rewritten to /new before route lookup.
	// s := celeris.New()
	// s.Pre(rewrite.New(rewrite.Config{...}))
	_ = rewrite.New(rewrite.Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
	})
}

func ExampleNew_captureGroups() {
	// Capture groups: extract user ID and rewrite to an API path.
	_ = rewrite.New(rewrite.Config{
		Rules: map[string]string{
			`^/users/(\d+)/posts$`: "/api/v2/users/$1/posts",
		},
	})
}

func ExampleNew_redirect() {
	// Redirect mode: send a 301 redirect instead of a silent rewrite.
	_ = rewrite.New(rewrite.Config{
		Rules: map[string]string{
			"^/old$": "/new",
		},
		RedirectCode: 301,
	})
}
