package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeTagObjectChecker struct {
	tags map[string]bool // "owner/repo|sha-lower" → true if tag object
}

func (f *fakeTagObjectChecker) IsKnownTagObject(owner, repo, sha string) bool {
	return f.tags[owner+"/"+repo+"|"+sha]
}

func TestDepReleaseURL(t *testing.T) {
	const commitSHA = "11bd71901bbe5b1630ceea73d27597364c9af683"
	const tagObjectSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	tagChecker := &fakeTagObjectChecker{tags: map[string]bool{
		"actions/github-script|" + tagObjectSHA: true,
	}}

	tests := []struct {
		name    string
		dep     string
		checker tagObjectChecker
		want    string
	}{
		{
			name: "commit-sha pin → /commit/",
			dep:  "actions/checkout@" + commitSHA,
			want: "https://github.com/actions/checkout/commit/" + commitSHA,
		},
		{
			name:    "commit-sha pin with nil checker → /commit/",
			dep:     "actions/checkout@" + commitSHA,
			checker: nil,
			want:    "https://github.com/actions/checkout/commit/" + commitSHA,
		},
		{
			name:    "tag-object SHA pin → /tree/ (no 404)",
			dep:     "actions/github-script@" + tagObjectSHA,
			checker: tagChecker,
			want:    "https://github.com/actions/github-script/tree/" + tagObjectSHA,
		},
		{
			name:    "tag-object SHA without checker knowledge → falls back to /commit/",
			dep:     "actions/github-script@" + tagObjectSHA,
			checker: &fakeTagObjectChecker{},
			want:    "https://github.com/actions/github-script/commit/" + tagObjectSHA,
		},
		{
			name: "tag ref → /releases/tag/",
			dep:  "actions/checkout@v4",
			want: "https://github.com/actions/checkout/releases/tag/v4",
		},
		{
			name: "branch ref → /releases/tag/ (best-effort)",
			dep:  "actions/checkout@main",
			want: "https://github.com/actions/checkout/releases/tag/main",
		},
		{
			name: "path action, commit-sha → /commit/ on the repo (path stripped)",
			dep:  "actions/cache/save@" + commitSHA,
			want: "https://github.com/actions/cache/commit/" + commitSHA,
		},
		{
			name: "no ref → /releases",
			dep:  "actions/checkout",
			want: "https://github.com/actions/checkout/releases",
		},
		{
			name: "malformed dep (no slash) → empty",
			dep:  "checkout@v4",
			want: "",
		},
		{
			name: "empty dep → empty",
			dep:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, depReleaseURL(tt.dep, tt.checker))
		})
	}
}
