package stream

import "testing"

// declaredInvalid889 is DeclaredInvalid, spelled out so the file builds on a tree
// that has not got the name.
const declaredInvalid889 = int64(-2)

// celeris#889, review round 2: the content-length a StreamWriter's headers
// declare, as the response's ending is held to it.
func TestNoteStreamHeaderContentLength889(t *testing.T) {
	cl := func(v ...string) [][2]string {
		h := [][2]string{{"content-type", "text/plain"}}
		for _, x := range v {
			h = append(h, [2]string{"Content-Length", x})
		}
		return h
	}
	cases := []struct {
		name   string
		status int
		head   bool
		hdrs   [][2]string
		want   int64
	}{
		{"none", 200, false, cl(), -1},
		{"plain", 200, false, cl("100"), 100},
		{"zero", 200, false, cl("0"), 0},
		{"spaces", 200, false, cl(" 7 "), 7},
		{"leading zeros", 200, false, cl("0042"), 42},
		{"identical list", 200, false, cl("5, 5"), 5},
		{"identical lines", 200, false, cl("5", "5"), 5},
		{"differing list", 200, false, cl("5, 6"), declaredInvalid889},
		{"differing lines", 200, false, cl("5", "6"), declaredInvalid889},
		{"first valid, second not", 200, false, cl("5", "x"), declaredInvalid889},
		{"plus", 200, false, cl("+5"), declaredInvalid889},
		{"minus", 200, false, cl("-5"), declaredInvalid889},
		{"hex", 200, false, cl("0x5"), declaredInvalid889},
		{"fraction", 200, false, cl("5.0"), declaredInvalid889},
		{"empty", 200, false, cl(""), declaredInvalid889},
		{"comma only", 200, false, cl(","), declaredInvalid889},
		{"trailing comma", 200, false, cl("5,"), declaredInvalid889},
		{"19 digits", 200, false, cl("9999999999999999999"), declaredInvalid889},
		{"HEAD declares none", 200, true, cl("+5"), -1},
		{"204 declares none", 204, false, cl("+5"), -1},
		{"304 declares none", 304, false, cl("+5"), -1},
		{"1xx declares none", 103, false, cl("+5"), -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStream(1)
			s.IsHEAD = tc.head
			s.NoteStreamHeader(tc.status, tc.hdrs)
			if got := s.respDeclared.Load() - 1; got != tc.want {
				t.Errorf("declared = %d, want %d", got, tc.want)
			}
			s.headersSent.Store(true)
			o, _, declared := s.responseOutcome()
			wantOutcome := outcomeComplete
			if tc.want == declaredInvalid889 || tc.want > 0 {
				wantOutcome = outcomeShort // nothing was written
			}
			if o != wantOutcome || declared != tc.want {
				t.Errorf("outcome %d declared %d, want %d / %d", o, declared, wantOutcome, tc.want)
			}
		})
	}
}

// Over the declared length is not complete either.
func TestResponseOutcomeOverTheDeclaredLength889(t *testing.T) {
	s := NewStream(1)
	s.NoteStreamHeader(200, [][2]string{{"content-length", "3"}})
	s.headersSent.Store(true)
	s.respWritten = 5
	if o, w, d := s.responseOutcome(); o != outcomeShort || w != 5 || d != 3 {
		t.Errorf("outcome %d written %d declared %d, want short / 5 / 3", o, w, d)
	}
}
