package doctor

import "testing"

func TestIsMutableVersionTag(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		// Mutable version tags — should be narrowed
		{"v4", true},
		{"v4.2", true},
		{"v1", true},
		{"v10", true},

		// Full semver — not mutable
		{"v4.2.1", false},
		{"v1.0.0", false},

		// SHA refs — must never be treated as mutable version tags
		{"1e7e51e771db61008b38414a730f564565cf7c20", false},
		{"de0fac2e4500dabe0009e67214ff5f5447ce83dd", false},
		{"0000000000000000000000000000000000000000", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"1E7E51E771DB61008B38414A730F564565CF7C20", false}, // uppercase

		// Non-version refs
		{"main", false},
		{"develop", false},
		{"", false},
		{"v", false},
	}
	for _, tc := range cases {
		got := IsMutableVersionTag(tc.ref)
		if got != tc.want {
			t.Errorf("IsMutableVersionTag(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}
