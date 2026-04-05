package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// =============================================================
// 1. Compare FastHandler vs slog.TextHandler output
// =============================================================

func TestFastHandler_vs_TextHandler(t *testing.T) {
	now := time.Date(2025, 6, 15, 14, 30, 45, 123_000_000, time.UTC)
	rec := slog.NewRecord(now, slog.LevelInfo, "test message", 0)
	rec.AddAttrs(slog.String("key", "value"), slog.Int("num", 42))

	var fastBuf, textBuf bytes.Buffer

	fastH := NewFastHandler(&fastBuf, nil)
	textH := slog.NewTextHandler(&textBuf, &slog.HandlerOptions{})

	if err := fastH.Handle(context.TODO(), rec); err != nil {
		t.Fatalf("FastHandler.Handle: %v", err)
	}
	if err := textH.Handle(context.TODO(), rec); err != nil {
		t.Fatalf("TextHandler.Handle: %v", err)
	}

	fastOut := fastBuf.String()
	textOut := textBuf.String()

	t.Logf("FastHandler: %s", fastOut)
	t.Logf("TextHandler: %s", textOut)

	// Parse both into key=value maps for structural comparison.
	fastKV := parseKV(fastOut)
	textKV := parseKV(textOut)

	// Compare structural keys.
	for _, key := range []string{"time", "level", "msg", "key", "num"} {
		fv, fok := fastKV[key]
		tv, tok := textKV[key]
		if !fok {
			t.Errorf("FastHandler missing key %q", key)
			continue
		}
		if !tok {
			t.Errorf("TextHandler missing key %q (unexpected)", key)
			continue
		}
		if key == "time" {
			// Accept if both parse to the same time.
			ft, err1 := time.Parse(time.RFC3339Nano, fv)
			tt, err2 := time.Parse(time.RFC3339Nano, tv)
			if err1 != nil {
				t.Errorf("FastHandler time %q does not parse: %v", fv, err1)
			}
			if err2 != nil {
				t.Errorf("TextHandler time %q does not parse: %v", tv, err2)
			}
			if err1 == nil && err2 == nil && !ft.Equal(tt) {
				t.Errorf("time mismatch: fast=%v text=%v", ft, tt)
			}
			continue
		}
		if fv != tv {
			t.Errorf("key %q: fast=%q text=%q", key, fv, tv)
		}
	}
}

// =============================================================
// 2. WithAttrs immutability
// =============================================================

func TestWithAttrs_Immutable(t *testing.T) {
	var buf bytes.Buffer
	h1 := NewFastHandler(&buf, nil)
	h2 := h1.WithAttrs([]slog.Attr{slog.String("a", "1")})

	if h1 == h2 {
		t.Fatal("WithAttrs returned same pointer")
	}
	// Original must not have prefix.
	if len(h1.prefix) != 0 {
		t.Fatalf("original handler mutated: prefix=%q", h1.prefix)
	}
}

func TestWithAttrs_Empty(t *testing.T) {
	var buf bytes.Buffer
	h := NewFastHandler(&buf, nil)
	h2 := h.WithAttrs(nil)
	if h != h2 {
		t.Fatal("WithAttrs(nil) should return same handler")
	}
}

// =============================================================
// 3. WithGroup nesting
// =============================================================

func TestWithGroup_Nested(t *testing.T) {
	var buf bytes.Buffer
	base := NewFastHandler(&buf, nil)
	h := base.WithGroup("g1").WithGroup("g2")
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := slog.NewRecord(now, slog.LevelInfo, "test", 0)
	rec.AddAttrs(slog.String("k", "v"))
	if err := h.Handle(context.TODO(), rec); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Logf("nested group output: %s", out)
	if !strings.Contains(out, "g1.g2.k=v") {
		t.Errorf("expected nested group prefix g1.g2.k=v, got: %s", out)
	}
}

func TestWithGroup_WithAttrs_Mixed(t *testing.T) {
	var buf bytes.Buffer
	h := NewFastHandler(&buf, nil)
	h2 := h.WithGroup("grp").WithAttrs([]slog.Attr{slog.String("pre", "val")})

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := slog.NewRecord(now, slog.LevelInfo, "test", 0)
	rec.AddAttrs(slog.String("k", "v"))
	if err := h2.Handle(context.TODO(), rec); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Logf("group+attrs output: %s", out)
	if !strings.Contains(out, "grp.pre=val") {
		t.Errorf("expected grp.pre=val in output: %s", out)
	}
	if !strings.Contains(out, "grp.k=v") {
		t.Errorf("expected grp.k=v in output: %s", out)
	}
}

// =============================================================
// 4. appendTextValue edge cases
// =============================================================

func TestAppendTextValue_Edges(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", `""`},
		{"simple", "hello", "hello"},
		{"with space", "hello world", `"hello world"`},
		{"with equals", "a=b", `"a=b"`},
		{"with quote", `say "hi"`, `"say \"hi\""`},
		{"with backslash", `a\b`, `"a\\b"`},
		{"with newline", "line1\nline2", `"line1\nline2"`},
		{"with tab", "a\tb", `"a\tb"`},
		{"with CR", "a\rb", `"a\rb"`},
		{"unicode basic", "caf\u00e9", "caf\u00e9"},
		{"unicode with space", "caf\u00e9 latt\u00e9", "\"caf\u00e9 latt\u00e9\""},
		{"long string", strings.Repeat("x", 10000), strings.Repeat("x", 10000)},
		{"only spaces", "   ", `"   "`},
		{"null byte", "a\x00b", `"a\x00b"`},
		{"DEL char", "a\x7fb", `"a\x7fb"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf []byte
			buf = appendTextValue(buf, tc.input)
			got := string(buf)
			if got != tc.want {
				t.Errorf("appendTextValue(%q)\n  got:  %q\n  want: %q", tc.input, got, tc.want)
			}
		})
	}
}

// =============================================================
// 5. appendTime timezone edge cases
// =============================================================

func TestAppendTime_Timezones(t *testing.T) {
	tests := []struct {
		name   string
		tz     *time.Location
		expect string // expected suffix
	}{
		{"UTC", time.UTC, "Z"},
		{"EST -05:00", time.FixedZone("EST", -5*3600), "-05:00"},
		{"IST +05:30", time.FixedZone("IST", 5*3600+30*60), "+05:30"},
		{"Nepal +05:45", time.FixedZone("NPT", 5*3600+45*60), "+05:45"},
		{"Chatham +12:45", time.FixedZone("CHAST", 12*3600+45*60), "+12:45"},
		{"Marquesas -09:30", time.FixedZone("MART", -9*3600-30*60), "-09:30"},
	}

	base := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			local := base.In(tc.tz)
			var buf []byte
			buf = appendTime(buf, local)
			got := string(buf)
			if !strings.HasSuffix(got, tc.expect) {
				t.Errorf("appendTime in %s\n  got:  %q\n  want suffix: %q", tc.name, got, tc.expect)
			}
			// Verify the time parses back correctly.
			parsed, err := time.Parse("2006-01-02T15:04:05.000Z07:00", got)
			if err != nil {
				t.Fatalf("cannot parse output %q: %v", got, err)
			}
			if !parsed.Equal(base) {
				t.Errorf("round-trip mismatch: got %v, want %v", parsed, base)
			}
		})
	}
}

// =============================================================
// 6. appendDuration edge cases
// =============================================================

func TestAppendDuration_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		want    string
		skipBug bool // true = known bug, log and skip
	}{
		{"zero", 0, "0.0ns", false},
		{"1 nanosecond", time.Nanosecond, "1.0ns", false},
		{"999 nanoseconds", 999 * time.Nanosecond, "999.0ns", false},
		{"1 microsecond", time.Microsecond, "1.0\u00b5s", false},
		{"1 millisecond", time.Millisecond, "1.0ms", false},
		{"1 second", time.Second, "1.000s", false},
		{"very large", 24 * time.Hour, "86400.000s", false},
		{"sub-microsecond", 500 * time.Nanosecond, "500.0ns", false},
		{"1.5ms", 1500 * time.Microsecond, "1.5ms", false},
		// Negative durations fall back to Duration.String().
		{"negative 1ms", -time.Millisecond, "-1ms", false},
		{"negative 500ns", -500 * time.Nanosecond, "-500ns", false},
		{"negative 50us", -50 * time.Microsecond, "-50\u00b5s", false},
		{"negative 2s", -2 * time.Second, "-2s", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf []byte
			buf = appendDuration(buf, tc.d)
			got := string(buf)
			if got != tc.want {
				if tc.skipBug {
					t.Skipf("KNOWN BUG: appendDuration(%v) = %q, want %q", tc.d, got, tc.want)
				}
				t.Errorf("appendDuration(%v)\n  got:  %q\n  want: %q", tc.d, got, tc.want)
			}
		})
	}
}

// =============================================================
// 7 & 8. Buffer pool safety and reset
// =============================================================

// syncWriter serializes writes so the race detector does not flag
// bytes.Buffer (which is not goroutine-safe). This isolates the
// test to the sync.Pool path.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestBufferPool_ConcurrentSafety(t *testing.T) {
	var raw bytes.Buffer
	sw := &syncWriter{w: &raw}
	h := NewFastHandler(sw, nil)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := slog.NewRecord(now, slog.LevelInfo, fmt.Sprintf("msg-%d", idx), 0)
			rec.AddAttrs(slog.Int("idx", idx))
			_ = h.Handle(context.TODO(), rec)
		}(i)
	}
	wg.Wait()

	// Verify no panics and output has 100 lines.
	lines := strings.Split(strings.TrimSpace(raw.String()), "\n")
	if len(lines) != 100 {
		t.Errorf("expected 100 lines, got %d", len(lines))
	}
}

func TestBufferPool_NoLeakAcrossHandlers(t *testing.T) {
	// Two handlers writing to different outputs must not cross-contaminate.
	var buf1, buf2 bytes.Buffer
	h1 := NewFastHandler(&buf1, nil)
	h2 := NewFastHandler(&buf2, nil)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	for range 50 {
		rec1 := slog.NewRecord(now, slog.LevelInfo, "handler1", 0)
		rec2 := slog.NewRecord(now, slog.LevelWarn, "handler2", 0)
		_ = h1.Handle(context.TODO(), rec1)
		_ = h2.Handle(context.TODO(), rec2)
	}

	// h1 output must never contain "handler2" and vice versa.
	if strings.Contains(buf1.String(), "handler2") {
		t.Error("buf1 contains handler2 data -- buffer pool leak")
	}
	if strings.Contains(buf2.String(), "handler1") {
		t.Error("buf2 contains handler1 data -- buffer pool leak")
	}
}

// =============================================================
// 9. appendInt zero handling
// =============================================================

func TestAppendInt_Zero(t *testing.T) {
	tests := []struct {
		n     int
		width int
		want  string
	}{
		{0, 4, "0000"},
		{0, 2, "00"},
		{0, 1, "0"},
		{5, 2, "05"},
		{12, 2, "12"},
		{2025, 4, "2025"},
		{9, 4, "0009"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("n=%d,w=%d", tc.n, tc.width), func(t *testing.T) {
			var buf []byte
			buf = appendInt(buf, tc.n, tc.width)
			got := string(buf)
			if got != tc.want {
				t.Errorf("appendInt(%d, %d) = %q, want %q", tc.n, tc.width, got, tc.want)
			}
		})
	}
}

// =============================================================
// 10. Full format compatibility test
// =============================================================

func TestFastHandler_FormatCompat_AllTypes(t *testing.T) {
	now := time.Date(2025, 3, 20, 10, 15, 30, 500_000_000, time.UTC)
	dur := 2*time.Second + 500*time.Millisecond

	rec := slog.NewRecord(now, slog.LevelWarn, "all types", 0)
	rec.AddAttrs(
		slog.String("str", "hello"),
		slog.Int("int", -42),
		slog.Float64("float", 3.14),
		slog.Bool("bool", true),
		slog.Duration("dur", dur),
		slog.Time("ts", now),
		slog.String("quoted", "has space"),
		slog.Uint64("uint", 999),
	)

	var fastBuf bytes.Buffer
	h := NewFastHandler(&fastBuf, nil)
	if err := h.Handle(context.TODO(), rec); err != nil {
		t.Fatal(err)
	}
	out := fastBuf.String()
	t.Logf("output: %s", out)

	// Verify key fields.
	checks := map[string]string{
		"level=": "WARN",
		"str=":   "hello",
		"int=":   "-42",
		"float=": "3.14",
		"bool=":  "true",
		"uint=":  "999",
	}
	for prefix, want := range checks {
		idx := strings.Index(out, prefix)
		if idx < 0 {
			t.Errorf("missing %s in output", prefix)
			continue
		}
		// Extract value until space or newline.
		valStart := idx + len(prefix)
		valEnd := valStart
		for valEnd < len(out) && out[valEnd] != ' ' && out[valEnd] != '\n' {
			valEnd++
		}
		got := out[valStart:valEnd]
		if got != want {
			t.Errorf("%s: got %q, want %q", prefix, got, want)
		}
	}
}

// =============================================================
// Bonus: Duration format divergence from slog.TextHandler
// =============================================================

func TestDuration_FormatDivergence(t *testing.T) {
	// slog.TextHandler uses time.Duration.String() which produces "1ms".
	// FastHandler uses its own format which produces "1.0ms".
	// This test documents the divergence.
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	durs := []time.Duration{
		0,
		time.Nanosecond,
		time.Microsecond,
		time.Millisecond,
		time.Second,
		2*time.Second + 500*time.Millisecond,
	}

	for _, d := range durs {
		rec := slog.NewRecord(now, slog.LevelInfo, "test", 0)
		rec.AddAttrs(slog.Duration("d", d))

		var fastBuf, textBuf bytes.Buffer
		fastH := NewFastHandler(&fastBuf, nil)
		textH := slog.NewTextHandler(&textBuf, &slog.HandlerOptions{})
		_ = fastH.Handle(context.TODO(), rec)
		_ = textH.Handle(context.TODO(), rec)

		fastKV := parseKV(strings.TrimSpace(fastBuf.String()))
		textKV := parseKV(strings.TrimSpace(textBuf.String()))

		if fastKV["d"] != textKV["d"] {
			t.Logf("DIVERGENCE dur=%v: fast=%q text=%q", d, fastKV["d"], textKV["d"])
		}
	}
}

// =============================================================
// Bonus: Level filtering
// =============================================================

func TestFastHandler_Enabled(t *testing.T) {
	h := NewFastHandler(nil, &FastHandlerOptions{Level: slog.LevelWarn})
	if h.Enabled(context.TODO(), slog.LevelInfo) {
		t.Error("should not be enabled for INFO when level is WARN")
	}
	if !h.Enabled(context.TODO(), slog.LevelWarn) {
		t.Error("should be enabled for WARN")
	}
	if !h.Enabled(context.TODO(), slog.LevelError) {
		t.Error("should be enabled for ERROR")
	}
}

// =============================================================
// Helper: parse key=value pairs from slog output line
// =============================================================

func parseKV(line string) map[string]string {
	line = strings.TrimSpace(line)
	result := make(map[string]string)
	for len(line) > 0 {
		// Find key.
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			break
		}
		key := line[:eqIdx]
		line = line[eqIdx+1:]

		// Extract value.
		var val string
		if len(line) > 0 && line[0] == '"' {
			// Quoted value -- find closing quote (handling escapes).
			i := 1
			for i < len(line) {
				if line[i] == '\\' {
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				i++
			}
			val = line[:i]
			line = line[i:]
		} else {
			// Unquoted -- until space.
			spIdx := strings.IndexByte(line, ' ')
			if spIdx < 0 {
				val = line
				line = ""
			} else {
				val = line[:spIdx]
				line = line[spIdx:]
			}
		}

		result[key] = val
		line = strings.TrimLeft(line, " ")
	}
	return result
}
