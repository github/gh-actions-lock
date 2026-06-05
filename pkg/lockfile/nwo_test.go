package lockfile

import "testing"

func TestSplitNWO(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		owner string
		repo  string
		ok    bool
	}{
		{"empty", "", "", "", false},
		{"no slash", "owner", "", "", false},
		{"leading slash", "/repo", "", "", false},
		{"trailing slash", "owner/", "", "", false},
		{"only slash", "/", "", "", false},
		{"basic", "actions/checkout", "actions", "checkout", true},
		{"sub-path drops to repo", "actions/cache/save", "actions", "cache", true},
		{"deep sub-path drops to repo", "actions/cache/restore/extra", "actions", "cache", true},
		{"preserves casing", "Actions/Checkout", "Actions", "Checkout", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, ok := SplitNWO(tc.in)
			if owner != tc.owner || repo != tc.repo || ok != tc.ok {
				t.Fatalf("SplitNWO(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, owner, repo, ok, tc.owner, tc.repo, tc.ok)
			}
		})
	}
}
