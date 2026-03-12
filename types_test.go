package celeris

import "testing"

func TestSameSiteString(t *testing.T) {
	tests := []struct {
		ss   SameSite
		want string
	}{
		{SameSiteDefaultMode, ""},
		{SameSiteLaxMode, "Lax"},
		{SameSiteStrictMode, "Strict"},
		{SameSiteNoneMode, "None"},
		{SameSite(99), ""},
	}
	for _, tt := range tests {
		if got := tt.ss.String(); got != tt.want {
			t.Errorf("SameSite(%d).String() = %q, want %q", tt.ss, got, tt.want)
		}
	}
}
