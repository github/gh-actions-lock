package doctor

import (
	"fmt"
	"testing"
)

func TestIsUpgrade_Cases(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v4.0.0", "v4", false},
		{"v4", "v4", false},
		{"v3.1.1", "v3", false},
		{"v3.0.1", "v3", false},
		{"v2.2.0", "v2", false},
		{"v2.0.0", "v2", false},
		{"v2.2.1", "v2.2.2", true},
		{"v5", "v6", true},
		{"main", "v1.1.0", true},
		{"v4", "codeql-bundle-v2.6.0-beta.1", false},
		{"v4.0.0", "v4.0.0", false},
		{"v1.1.0", "v1.1.0", false},
		{"v3", "v3.35.2", true},
	}
	for _, tc := range cases {
		got := IsUpgrade(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("IsUpgrade(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		} else {
			fmt.Printf("✓ %-20s → %-30s  upgrade=%v\n", tc.current, tc.latest, got)
		}
	}
}

func TestIsNarrowedVersion(t *testing.T) {
	cases := []struct {
		mutable, narrowed string
		want              bool
	}{
		{"v4", "v4.1.0", true},
		{"v4", "v4.0.0", true},
		{"v4.2", "v4.2.1", true},
		{"v4.2", "v4.2.0", true},
		{"v4", "v5.0.0", false},      // different major
		{"v4.2", "v4.3.0", false},    // different minor
		{"v4.1.0", "v4.1.0", true},   // identity (full semver is its own narrowing)
		{"v4", "v4", false},          // mutable→mutable, not narrowed
		{"main", "v4.1.0", false},    // non-version
		{"v4", "v4.1.0-beta", false}, // pre-release
	}
	for _, tc := range cases {
		got := IsNarrowedVersion(tc.mutable, tc.narrowed)
		if got != tc.want {
			t.Errorf("IsNarrowedVersion(%q, %q) = %v, want %v", tc.mutable, tc.narrowed, got, tc.want)
		}
	}
}
