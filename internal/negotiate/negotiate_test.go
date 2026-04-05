package negotiate

import "testing"

func TestMatchMedia(t *testing.T) {
	tests := []struct {
		pattern string
		offer   string
		want    bool
	}{
		{"*/*", "text/html", true},
		{"*", "text/html", true},
		{"*", "application/json", true},
		{"text/*", "text/html", true},
		{"text/*", "text/plain", true},
		{"text/*", "application/json", false},
		{"application/json", "application/json", true},
		{"application/json", "text/html", false},
		{"noslash", "noslash", true},
		{"noslash", "other", false},
		{"text/html", "text/plain", false},
		{"", "", true},
	}
	for _, tt := range tests {
		got := MatchMedia(tt.pattern, tt.offer)
		if got != tt.want {
			t.Errorf("MatchMedia(%q, %q) = %v, want %v", tt.pattern, tt.offer, got, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		header string
		want   []AcceptItem
	}{
		{
			"application/json",
			[]AcceptItem{{MediaType: "application/json", Quality: 1.0}},
		},
		{
			"text/html, application/json;q=0.9, */*;q=0.1",
			[]AcceptItem{
				{MediaType: "text/html", Quality: 1.0},
				{MediaType: "application/json", Quality: 0.9},
				{MediaType: "*/*", Quality: 0.1},
			},
		},
		{"", nil},
		{"   ", nil},
		{",,,", nil},
		{
			"text/html;q=abc",
			[]AcceptItem{{MediaType: "text/html", Quality: 1.0}},
		},
		{
			"text/html;q=0.5;level=1",
			[]AcceptItem{{MediaType: "text/html", Quality: 0.5}},
		},
		{
			"*",
			[]AcceptItem{{MediaType: "*", Quality: 1.0}},
		},
		{
			"application/json;q=0",
			[]AcceptItem{{MediaType: "application/json", Quality: 0.0}},
		},
	}
	for _, tt := range tests {
		got := Parse(tt.header)
		if len(got) != len(tt.want) {
			t.Errorf("Parse(%q) returned %d items, want %d", tt.header, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i].MediaType != tt.want[i].MediaType {
				t.Errorf("Parse(%q)[%d].MediaType = %q, want %q", tt.header, i, got[i].MediaType, tt.want[i].MediaType)
			}
			if got[i].Quality != tt.want[i].Quality {
				t.Errorf("Parse(%q)[%d].Quality = %f, want %f", tt.header, i, got[i].Quality, tt.want[i].Quality)
			}
		}
	}
}

func TestAccept(t *testing.T) {
	tests := []struct {
		header string
		offers []string
		want   string
	}{
		{"application/json", []string{"application/json"}, "application/json"},
		{"text/html, application/json;q=0.9", []string{"application/json", "text/html"}, "text/html"},
		{"*/*", []string{"text/plain"}, "text/plain"},
		{"", []string{"application/json"}, ""},
		{"text/html;q=0.5, text/plain;q=0.9", []string{"text/plain", "text/html"}, "text/plain"},
		// bare * wildcard
		{"*", []string{"application/json"}, "application/json"},
		{"*;q=0.5, text/html", []string{"text/html", "text/plain"}, "text/html"},
	}
	for _, tt := range tests {
		got := Accept(tt.header, tt.offers)
		if got != tt.want {
			t.Errorf("Accept(%q, %v) = %q, want %q", tt.header, tt.offers, got, tt.want)
		}
	}
}

func TestAcceptRejectsQZero(t *testing.T) {
	// q=0 means "not acceptable" per RFC 9110.
	tests := []struct {
		name   string
		header string
		offers []string
		want   string
	}{
		{
			"single q=0 is not acceptable",
			"text/html;q=0",
			[]string{"text/html"},
			"",
		},
		{
			"q=0.0 is not acceptable",
			"text/html;q=0.0",
			[]string{"text/html"},
			"",
		},
		{
			"q=0 excluded, fallback to other",
			"text/html;q=0, application/json;q=0.5",
			[]string{"text/html", "application/json"},
			"application/json",
		},
		{
			"all q=0 means no match",
			"text/html;q=0, application/json;q=0",
			[]string{"text/html", "application/json"},
			"",
		},
		{
			"wildcard q=0 is not acceptable",
			"*/*;q=0",
			[]string{"text/html"},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Accept(tt.header, tt.offers)
			if got != tt.want {
				t.Errorf("Accept(%q, %v) = %q, want %q", tt.header, tt.offers, got, tt.want)
			}
		})
	}
}

func TestAcceptQZeroWildcardExclusion(t *testing.T) {
	tests := []struct {
		name   string
		header string
		offers []string
		want   string
	}{
		{
			"Accept-Encoding wildcard with br;q=0 excludes br",
			"*, br;q=0",
			[]string{"gzip", "br"},
			"gzip",
		},
		{
			"Accept wildcard with text/html;q=0 excludes text/html",
			"*/*, text/html;q=0",
			[]string{"application/json", "text/html"},
			"application/json",
		},
		{
			"Accept wildcard with text/html;q=0 does not match text/html",
			"*/*, text/html;q=0",
			[]string{"text/html"},
			"",
		},
		{
			"Accept wildcard with text/html;q=0 matches other types",
			"*/*, text/html;q=0",
			[]string{"text/plain"},
			"text/plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Accept(tt.header, tt.offers)
			if got != tt.want {
				t.Errorf("Accept(%q, %v) = %q, want %q", tt.header, tt.offers, got, tt.want)
			}
		})
	}
}

func TestMatchMediaBareWildcard(t *testing.T) {
	// bare "*" should match any offer, including media types with slashes
	if !MatchMedia("*", "text/html") {
		t.Error("MatchMedia(\"*\", \"text/html\") should be true")
	}
	if !MatchMedia("*", "application/json") {
		t.Error("MatchMedia(\"*\", \"application/json\") should be true")
	}
	if !MatchMedia("*", "noslash") {
		t.Error("MatchMedia(\"*\", \"noslash\") should be true")
	}
}
