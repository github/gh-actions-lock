package ghapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidRefName(t *testing.T) {
	valid := []string{"v4", "v4.2.1", "main", "releases/v4", "feature/x-y_z", "v1.0.0-rc.1"}
	invalid := []string{
		"", "@", "main~1", "v1^", "v1^{commit}", "v1@{1}", "a..b", "refs:x", "a?b", "a*b", "a[b", `a\b`,
		"a b", "a\tb", "a\x7fb", "-x", "/x", "x/", "x.", "x//y", ".x", "x/.y", "x.lock", "x.lock/y", "\xff",
	}
	for _, name := range valid {
		assert.True(t, ValidRefName(name), name)
	}
	for _, name := range invalid {
		assert.False(t, ValidRefName(name), "%q", name)
	}
}

func TestAcceptedRefName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"v6.1.0", true},
		{"v6.1.0-1-g373d742f768e", false},
		{"v1-g1234", false},
		{"release-gABCD", false},
		{"v1-gabc", true},  // three hex digits
		{"v1-gxyz1", true}, // not hex
		{"v1-G1234", true}, // uppercase G
		{"v1-g1234-rc", true},
		{"main~1", false},
		{"rel@1", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, AcceptedRefName(tt.name), tt.name)
	}
}

func TestDescribeName(t *testing.T) {
	for name, want := range map[string]bool{
		"v1.0.0-1-gabcd1234": true,
		"v1-gabc":            false,
		"main~1":             false,
		"v1.0.0":             false,
	} {
		assert.Equal(t, want, DescribeName(name), name)
	}
}

func TestRecordableRefName(t *testing.T) {
	for name, want := range map[string]bool{
		"v4":                    true,
		"releases/v4":           true,
		"":                      false,
		"refs/release":          false,
		strings.Repeat("a", 40): false,
		strings.Repeat("A", 64): false,
		strings.Repeat("a", 39): true,
		"release-g1234":         false,
		"main~1":                false,
	} {
		assert.Equal(t, want, RecordableRefName(name), "%q", name)
	}
}
