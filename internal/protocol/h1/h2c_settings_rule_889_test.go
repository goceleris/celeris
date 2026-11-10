package h1

import "testing"

// RFC 7540 section 3.2.1 as the parser applies it (celeris#889, items 41 and
// 43): UpgradeH2C needs exactly one HTTP2-Settings header line, whose value
// may be empty. A missing or repeated one never upgrades.
func TestParseRequest_H2CSettingsLines889(t *testing.T) {
	const head = "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"
	const v = "AAMAAABkAAQAoAAAAAIAAAAA"
	cases := []struct {
		name    string
		lines   string
		want    bool
		wantSet string
	}{
		{"one", "HTTP2-Settings: " + v + "\r\n", true, v},
		{"one lowercase", "http2-settings: " + v + "\r\n", true, v},
		{"one with OWS", "HTTP2-Settings: \t " + v + " \t\r\n", true, v},
		{"one empty", "HTTP2-Settings: \r\n", true, ""},
		{"one empty, no space", "HTTP2-Settings:\r\n", true, ""},
		{"one empty, only OWS", "HTTP2-Settings: \t \r\n", true, ""},
		{"missing", "", false, ""},
		{"two valid", "HTTP2-Settings: " + v + "\r\nHTTP2-Settings: " + v + "\r\n", false, ""},
		{"valid then empty", "HTTP2-Settings: " + v + "\r\nHTTP2-Settings:\r\n", false, ""},
		{"empty then valid", "HTTP2-Settings:\r\nHTTP2-Settings: " + v + "\r\n", false, ""},
		{"two empty", "HTTP2-Settings:\r\nHTTP2-Settings:\r\n", false, ""},
		{"two, mixed case", "HTTP2-Settings: " + v + "\r\nhttp2-SETTINGS: " + v + "\r\n", false, ""},
		{"three", "HTTP2-Settings: " + v + "\r\nHTTP2-Settings: " + v + "\r\nHTTP2-Settings: " + v + "\r\n", false, ""},
		{"repeated with another header between", "HTTP2-Settings: " + v + "\r\nAccept: */*\r\nHTTP2-Settings: " + v + "\r\n", false, ""},
	}
	for _, mode := range []struct {
		name string
		set  func(*Parser)
	}{
		{"strings", func(*Parser) {}},
		{"zero-copy", func(p *Parser) { p.SetZeroCopy(true) }},
	} {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				p := NewParser()
				mode.set(p)
				p.Reset([]byte(head + tc.lines + "\r\n"))
				var req Request
				if _, err := p.ParseRequest(&req); err != nil {
					t.Fatalf("parse error: %v", err)
				}
				if req.UpgradeH2C != tc.want {
					t.Fatalf("UpgradeH2C = %v, want %v", req.UpgradeH2C, tc.want)
				}
				if tc.want && req.HTTP2Settings != tc.wantSet {
					t.Fatalf("HTTP2Settings = %q, want %q", req.HTTP2Settings, tc.wantSet)
				}
			})
		}
	}
}

// The repeated-line latch is per request: a request that repeated the header
// does not make the next one on the same parser (a keep-alive conn) fail to
// upgrade, nor the reverse.
func TestParseRequest_H2CSettingsLatchResets889(t *testing.T) {
	const head = "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"
	one := head + "HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"
	two := head + "HTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\nHTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"
	p := NewParser()
	for i, tc := range []struct {
		raw  string
		want bool
	}{{two, false}, {one, true}, {two, false}, {one, true}, {one, true}} {
		p.Reset([]byte(tc.raw))
		var req Request
		if _, err := p.ParseRequest(&req); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if req.UpgradeH2C != tc.want {
			t.Fatalf("request %d: UpgradeH2C = %v, want %v", i, req.UpgradeH2C, tc.want)
		}
	}
}

// Detection off (EnableH2Upgrade = false): nothing is counted, nothing upgrades.
func TestParseRequest_H2CSettingsDetectDisabled889(t *testing.T) {
	p := NewParser()
	p.SetDisableH2CDetect(true)
	p.Reset([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAoAAAAAIAAAAA\r\n\r\n"))
	var req Request
	if _, err := p.ParseRequest(&req); err != nil {
		t.Fatal(err)
	}
	if req.UpgradeH2C {
		t.Fatal("UpgradeH2C with detection disabled")
	}
}
