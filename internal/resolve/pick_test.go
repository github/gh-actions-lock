package resolve

import "testing"

func TestHintMatch(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		hint       string
		want       string
	}{
		{"empty hint", []string{"a", "b"}, "", ""},
		{"hint present", []string{"a", "b", "c"}, "b", "b"},
		{"hint absent", []string{"a", "b"}, "z", ""},
		{"nil candidates", nil, "a", ""},
		{"empty candidates", []string{}, "a", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hintMatch(tt.candidates, tt.hint)
			if got != tt.want {
				t.Errorf("hintMatch(%v, %q) = %q, want %q", tt.candidates, tt.hint, got, tt.want)
			}
		})
	}
}

func TestPickPreferred(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		hint       string
		fallback   string
		want       string
	}{
		{"empty candidates", nil, "a", "b", ""},
		{"hint wins", []string{"a", "b", "c"}, "b", "a", "b"},
		{"fallback wins", []string{"a", "b", "c"}, "z", "c", "c"},
		{"lex first", []string{"c", "a", "b"}, "", "", "a"},
		{"fallback absent falls to lex", []string{"c", "b"}, "", "z", "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickPreferred(tt.candidates, tt.hint, tt.fallback)
			if got != tt.want {
				t.Errorf("pickPreferred(%v, %q, %q) = %q, want %q",
					tt.candidates, tt.hint, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestPickPreferredTag(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		hint       string
		want       string
	}{
		{"hint wins over semver", []string{"v1.0.0", "v2.0.0"}, "v1.0.0", "v1.0.0"},
		{"highest full semver", []string{"v1.0.0", "v3.2.1", "v2.0.0"}, "", "v3.2.1"},
		{"full semver beats major-only", []string{"v5", "v4.3.1"}, "", "v4.3.1"},
		{"full semver beats higher major-only", []string{"v9", "v2.1.0"}, "", "v2.1.0"},
		{"major-only when no full semver", []string{"v4", "v3"}, "", "v4"},
		{"no semver falls to lex", []string{"beta", "alpha"}, "", "alpha"},
		{"mixed semver and non-semver", []string{"latest", "v1.0.0", "v2.0.0"}, "", "v2.0.0"},
		{"single candidate", []string{"v1.0.0"}, "", "v1.0.0"},
		{"empty candidates", nil, "v1.0.0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickPreferredTag(tt.candidates, tt.hint)
			if got != tt.want {
				t.Errorf("pickPreferredTag(%v, %q) = %q, want %q",
					tt.candidates, tt.hint, got, tt.want)
			}
		})
	}
}
