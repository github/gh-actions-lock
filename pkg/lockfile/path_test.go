package lockfile

import "testing"

func TestUsesIndexKey(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOk bool
	}{
		{"basic", "actions/checkout@v4", "actions/checkout@v4", true},
		{"sub-action path collapses", "actions/cache/save@v4", "actions/cache@v4", true},
		{"deep sub-action path", "actions/cache/restore/extra@v4", "actions/cache@v4", true},
		{"case-insensitive owner/repo", "Actions/Checkout@v4", "actions/checkout@v4", true},
		{"local action ./", "./local", "", false},
		{"local action .\\", ".\\local", "", false},
		{"docker", "docker://image:tag", "", false},
		{"missing @", "actions/checkout", "", false},
		{"trailing @", "actions/checkout@", "", false},
		{"missing slash", "checkout@v4", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := usesIndexKey(tc.in)
			if ok != tc.wantOk || got != tc.want {
				t.Fatalf("usesIndexKey(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOk)
			}
		})
	}
}
