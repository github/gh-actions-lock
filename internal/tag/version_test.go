package tag

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantOK     bool
		wantFamily string
		wantFull   bool
		wantMajor  string
	}{
		{name: "unprefixed full", in: "v1.2.3", wantOK: true, wantFull: true, wantMajor: "v1"},
		{name: "bare and v tags share a family", in: "1.2.3", wantOK: true, wantFull: true, wantMajor: "1"},
		{name: "path-prefixed full", in: "actions/verify/v0.5.1", wantOK: true, wantFamily: "actions/verify/", wantFull: true, wantMajor: "actions/verify/v0"},
		{name: "path-prefixed major", in: "actions/verify/v0", wantOK: true, wantFamily: "actions/verify/", wantMajor: "actions/verify/v0"},
		{name: "path without v joins the v family", in: "tools/1.0.0", wantOK: true, wantFamily: "tools/", wantFull: true, wantMajor: "tools/1"},
		{name: "prefixed pre-release", in: "a/v1.0.0-rc.1", wantOK: true, wantFamily: "a/", wantMajor: "a/v1"},
		{name: "branch name with digit is not a version", in: "feature/2fa"},
		{name: "non-version path", in: "releases/main"},
		{name: "plain branch", in: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sv, ok := ParseVersion(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if sv.Raw != tt.in {
				t.Errorf("Raw = %q, want %q", sv.Raw, tt.in)
			}
			if got := Family(sv); got != tt.wantFamily {
				t.Errorf("Family = %q, want %q", got, tt.wantFamily)
			}
			if sv.IsFull() != tt.wantFull {
				t.Errorf("IsFull = %v, want %v", sv.IsFull(), tt.wantFull)
			}
			if got := sv.MajorTag(); got != tt.wantMajor {
				t.Errorf("MajorTag = %q, want %q", got, tt.wantMajor)
			}
		})
	}
}
