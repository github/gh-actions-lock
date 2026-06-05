package main

import (
	"testing"

	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/stretchr/testify/assert"
)

// tagChecker returns a TagObjectCheck backed by an in-memory set.
func tagChecker(tags map[string]bool) format.TagObjectCheck {
	return func(owner, repo, sha string) bool {
		return tags[owner+"/"+repo+"|"+sha]
	}
}

func TestDepReleaseURL(t *testing.T) {
	const commitSHA = "11bd71901bbe5b1630ceea73d27597364c9af683"
	const tagObjectSHA = "d746ffe35508b1917358783b479e04febd2b8f71"
	knowsTag := tagChecker(map[string]bool{
		"actions/github-script|" + tagObjectSHA: true,
	})
	emptyChecker := tagChecker(nil)

	tests := []struct {
		name        string
		dep         string
		isTagObject format.TagObjectCheck
		want        string
	}{
		{
			name: "commit-sha pin → /commit/",
			dep:  "actions/checkout@" + commitSHA,
			want: "https://github.com/actions/checkout/commit/" + commitSHA,
		},
		{
			name:        "commit-sha pin with nil checker → /commit/",
			dep:         "actions/checkout@" + commitSHA,
			isTagObject: nil,
			want:        "https://github.com/actions/checkout/commit/" + commitSHA,
		},
		{
			name:        "tag-object SHA pin → /tree/ (no 404)",
			dep:         "actions/github-script@" + tagObjectSHA,
			isTagObject: knowsTag,
			want:        "https://github.com/actions/github-script/tree/" + tagObjectSHA,
		},
		{
			name:        "tag-object SHA without checker knowledge → falls back to /commit/",
			dep:         "actions/github-script@" + tagObjectSHA,
			isTagObject: emptyChecker,
			want:        "https://github.com/actions/github-script/commit/" + tagObjectSHA,
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
			assert.Equal(t, tt.want, format.DepReleaseURL(tt.dep, tt.isTagObject))
		})
	}
}
