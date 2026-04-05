package negotiate

import "testing"

func TestAcceptBasic(t *testing.T) {
	tests := []struct {
		name   string
		header string
		offers []string
		want   string
	}{
		{"exact match", "text/html", []string{"text/html"}, "text/html"},
		{"no match", "text/html", []string{"application/json"}, ""},
		{"wildcard", "*/*", []string{"application/json"}, "application/json"},
		{"quality preference", "text/html;q=0.9, application/json", []string{"text/html", "application/json"}, "application/json"},
		{"empty header", "", []string{"text/html"}, ""},
		{"empty offers", "text/html", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Accept(tt.header, tt.offers)
			if got != tt.want {
				t.Fatalf("Accept(%q, %v) = %q, want %q", tt.header, tt.offers, got, tt.want)
			}
		})
	}
}

func TestAcceptCaseInsensitive(t *testing.T) {
	tests := []struct {
		name   string
		header string
		offers []string
		want   string
	}{
		{"uppercase header encoding", "GZIP", []string{"gzip"}, "gzip"},
		{"mixed case header encoding", "Gzip, Deflate", []string{"gzip", "deflate"}, "gzip"},
		{"uppercase offer", "gzip", []string{"GZIP"}, "GZIP"},
		{"mixed media type", "TEXT/HTML", []string{"text/html"}, "text/html"},
		{"upper offer media", "text/html", []string{"TEXT/HTML"}, "TEXT/HTML"},
		{"wildcard subtype case", "TEXT/*", []string{"text/plain"}, "text/plain"},
		{"accept-encoding GZIP with offers gzip", "GZIP", []string{"gzip"}, "gzip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Accept(tt.header, tt.offers)
			if got != tt.want {
				t.Fatalf("Accept(%q, %v) = %q, want %q", tt.header, tt.offers, got, tt.want)
			}
		})
	}
}

func TestMatchMediaCaseInsensitive(t *testing.T) {
	tests := []struct {
		pattern string
		offer   string
		want    bool
	}{
		{"text/html", "TEXT/HTML", true},
		{"TEXT/HTML", "text/html", true},
		{"text/*", "TEXT/PLAIN", true},
		{"*/*", "anything", true},
		{"*", "gzip", true},
		{"gzip", "GZIP", true},
		{"text/html", "text/plain", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.offer, func(t *testing.T) {
			got := MatchMedia(tt.pattern, tt.offer)
			if got != tt.want {
				t.Fatalf("MatchMedia(%q, %q) = %v, want %v", tt.pattern, tt.offer, got, tt.want)
			}
		})
	}
}

func TestParseLowercasesMediaType(t *testing.T) {
	entries := Parse("TEXT/HTML, APPLICATION/JSON;q=0.9")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].MediaType != "text/html" {
		t.Fatalf("expected text/html, got %q", entries[0].MediaType)
	}
	if entries[1].MediaType != "application/json" {
		t.Fatalf("expected application/json, got %q", entries[1].MediaType)
	}
	if entries[1].Quality != 0.9 {
		t.Fatalf("expected quality 0.9, got %f", entries[1].Quality)
	}
}

func TestAcceptEncodingCaseInsensitive(t *testing.T) {
	// Simulate Accept-Encoding: GZIP with offers ["gzip"]
	got := Accept("GZIP", []string{"gzip"})
	if got != "gzip" {
		t.Fatalf("Accept(\"GZIP\", [\"gzip\"]) = %q, want \"gzip\"", got)
	}

	// Simulate Accept-Encoding: gzip, DEFLATE with offers ["deflate", "gzip"]
	got = Accept("gzip, DEFLATE", []string{"deflate", "gzip"})
	if got != "gzip" {
		t.Fatalf("expected gzip (first in header), got %q", got)
	}
}

func TestParseEmptyAndWhitespace(t *testing.T) {
	entries := Parse("  ,  , ")
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for whitespace-only input, got %d", len(entries))
	}
}

func TestAcceptQualityZeroExcluded(t *testing.T) {
	got := Accept("gzip;q=0, deflate", []string{"gzip", "deflate"})
	if got != "deflate" {
		t.Fatalf("expected deflate (gzip excluded by q=0), got %q", got)
	}
}
