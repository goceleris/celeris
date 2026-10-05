package probe

import "testing"

func TestParseKernelVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    KernelVersion
		wantErr bool
	}{
		{
			name:  "standard",
			input: "5.15.0",
			want:  KernelVersion{Major: 5, Minor: 15, Patch: 0},
		},
		{
			name:  "with suffix",
			input: "5.15.0-generic",
			want:  KernelVersion{Major: 5, Minor: 15, Patch: 0, Extra: "-generic"},
		},
		{
			name:  "complex suffix",
			input: "6.1.0-18-generic",
			want:  KernelVersion{Major: 6, Minor: 1, Patch: 0, Extra: "-18-generic"},
		},
		{
			name:  "minimal",
			input: "5.10",
			want:  KernelVersion{Major: 5, Minor: 10},
		},
		{
			name:  "with trailing space and extra info",
			input: "5.15.0-generic #1 SMP",
			want:  KernelVersion{Major: 5, Minor: 15, Patch: 0, Extra: "-generic"},
		},
		{
			name:  "patch only digits",
			input: "5.10.102",
			want:  KernelVersion{Major: 5, Minor: 10, Patch: 102},
		},
		{
			name:    "invalid letters",
			input:   "abc",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseKernelVersion(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKernelVersionAtLeast(t *testing.T) {
	kv := KernelVersion{Major: 5, Minor: 10, Patch: 0}

	tests := []struct {
		name  string
		major int
		minor int
		want  bool
	}{
		{"below minor", 5, 9, true},
		{"exact", 5, 10, true},
		{"above minor", 5, 11, false},
		{"above major", 6, 0, false},
		{"below major", 4, 20, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := kv.AtLeast(tt.major, tt.minor); got != tt.want {
				t.Fatalf("KernelVersion(%v).AtLeast(%d, %d) = %v, want %v", kv, tt.major, tt.minor, got, tt.want)
			}
		})
	}
}

func TestKernelVersionString(t *testing.T) {
	tests := []struct {
		kv   KernelVersion
		want string
	}{
		{KernelVersion{5, 15, 0, ""}, "5.15.0"},
		{KernelVersion{5, 15, 0, "-generic"}, "5.15.0-generic"},
		{KernelVersion{6, 1, 0, "-18-generic"}, "6.1.0-18-generic"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.kv.String(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
