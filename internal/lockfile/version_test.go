package lockfile

import "testing"

func TestParseSemver_RejectsOverflow(t *testing.T) {
	// A numeric component that overflows int must not parse: Atoi returns
	// MaxInt on range error, which would otherwise let a crafted tag
	// masquerade as the highest version.
	overflow := []string{
		"99999999999999999999.0.0",
		"v1.99999999999999999999.0",
		"v1.0.99999999999999999999",
	}
	for _, tag := range overflow {
		if _, ok := ParseVersion(tag); ok {
			t.Errorf("ParseVersion(%q) = ok; want rejected", tag)
		}
	}
}

func TestVersion_Greater(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool // a.Greater(b)
	}{
		{"higher patch", "v1.2.4", "v1.2.3", true},
		{"lower patch", "v1.2.3", "v1.2.4", false},
		{"higher major beats lower v-prefix mismatch", "2.0.0", "v1.0.0", true},
		{"bare equals v on version, v wins", "1.2.3", "v1.2.3", false},
		{"v beats bare on tie", "v1.2.3", "1.2.3", true},
		{"stable beats prerelease", "v1.2.3", "v1.2.3-rc.1", true},
		{"prerelease loses to stable", "v1.2.3-rc.1", "v1.2.3", false},
		{"partial v1 vs full v1.0.0 tie, lexical raw", "v1", "v1.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := ParseVersion(tt.a)
			if !ok {
				t.Fatalf("ParseVersion(%q) failed", tt.a)
			}
			b, ok := ParseVersion(tt.b)
			if !ok {
				t.Fatalf("ParseVersion(%q) failed", tt.b)
			}
			if got := a.Greater(b); got != tt.want {
				t.Errorf("%q.Greater(%q) = %v; want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
