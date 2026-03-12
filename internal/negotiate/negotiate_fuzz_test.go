package negotiate

import "testing"

func FuzzParse(f *testing.F) {
	f.Add("application/json")
	f.Add("text/html, application/json;q=0.9, */*;q=0.1")
	f.Add("")
	f.Add("   ")
	f.Add(",,,")
	f.Add("text/html;q=abc")
	f.Add("text/html;q=")
	f.Add("text/html;q=1.0;level=1")
	f.Add("*/*")
	f.Add("text/*")
	f.Add("application/json;q=0.0")
	f.Add("text/html;q=999")
	f.Fuzz(func(_ *testing.T, header string) {
		Parse(header) // must not panic
	})
}

func FuzzMatchMedia(f *testing.F) {
	f.Add("application/json", "application/json")
	f.Add("*/*", "text/html")
	f.Add("text/*", "text/html")
	f.Add("", "")
	f.Add("noslash", "noslash")
	f.Add("text/", "text/html")
	f.Add("/html", "/html")
	f.Fuzz(func(_ *testing.T, pattern, offer string) {
		MatchMedia(pattern, offer) // must not panic
	})
}

func FuzzAccept(f *testing.F) {
	f.Add("application/json", "application/json")
	f.Add("text/html, application/json;q=0.9", "application/json")
	f.Add("*/*", "text/plain")
	f.Add("", "application/json")
	f.Add("text/html;q=0.5, text/plain;q=0.9", "text/plain")
	f.Fuzz(func(_ *testing.T, header, offer string) {
		Accept(header, []string{offer}) // must not panic
	})
}
