package repospec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  Spec
		isErr bool
	}{
		{name: "owner/repo", in: "octo/api", want: Spec{Owner: "octo", Repo: "api"}},
		{name: "with ref", in: "octo/api@v1.2.3", want: Spec{Owner: "octo", Repo: "api", Ref: "v1.2.3"}},
		{name: "ref containing slash", in: "octo/api@feature/x", want: Spec{Owner: "octo", Repo: "api", Ref: "feature/x"}},
		{name: "host", in: "Example.GHE.com/octo/api", want: Spec{Host: "example.ghe.com", Owner: "octo", Repo: "api"}},
		{name: "host and ref", in: "example.ghe.com/octo/api@main", want: Spec{Host: "example.ghe.com", Owner: "octo", Repo: "api", Ref: "main"}},
		{name: "trailing .git", in: "octo/api.git", want: Spec{Owner: "octo", Repo: "api"}},
		{name: "url", in: "https://github.com/octo/api", want: Spec{Host: "github.com", Owner: "octo", Repo: "api"}},
		{name: "url with .git", in: "https://github.com/octo/api.git", want: Spec{Host: "github.com", Owner: "octo", Repo: "api"}},
		{name: "url trailing slash", in: "https://acme.ghe.com/octo/api/", want: Spec{Host: "acme.ghe.com", Owner: "octo", Repo: "api"}},
		{name: "tree url sets ref", in: "https://github.com/octo/api/tree/release/1.x", want: Spec{Host: "github.com", Owner: "octo", Repo: "api", Ref: "release/1.x"}},
		{name: "release url sets tag ref", in: "https://github.com/github/gh-actions-lock/releases/tag/v0.2.0", want: Spec{Host: "github.com", Owner: "github", Repo: "gh-actions-lock", Ref: "v0.2.0"}},
		{name: "release url with slashed tag", in: "https://github.com/octo/api/releases/tag/rel/1.0", want: Spec{Host: "github.com", Owner: "octo", Repo: "api", Ref: "rel/1.0"}},
		{name: "commit url sets sha ref", in: "https://github.com/github/gh-actions-lock/commit/d5dca22a06ff8590d50b41612038996800cfb336", want: Spec{Host: "github.com", Owner: "github", Repo: "gh-actions-lock", Ref: "d5dca22a06ff8590d50b41612038996800cfb336"}},
		{name: "releases listing rejected", in: "https://github.com/octo/api/releases", isErr: true},
		{name: "release tag without a tag rejected", in: "https://github.com/octo/api/releases/tag", isErr: true},
		{name: "commit url without sha rejected", in: "https://github.com/octo/api/commit", isErr: true},
		{name: "commit url with trailing path rejected", in: "https://github.com/octo/api/commit/abc/files", isErr: true},
		{name: "blob url rejected", in: "https://github.com/octo/api/blob/main/a.yml", isErr: true},
		{name: "url without repo", in: "https://github.com/octo", isErr: true},
		{name: "empty", in: "", isErr: true},
		{name: "single segment", in: "octo", isErr: true},
		{name: "too many segments", in: "a/b/c/d", isErr: true},
		{name: "empty ref", in: "octo/api@", isErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)
			if tt.isErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLooksLikeRepo(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"octo/api", true},
		{"octo/api@main", true},
		{"example.ghe.com/octo/api", true},
		{"https://github.com/octo/api", true},
		{".github/workflows/ci.yml", false},
		{"workflows/ci.yml", false},
		{"octo/ci.yaml", false},
		{"octo/ci.yml@main", false},
		{"./octo/api", false},
		{`octo\api`, false},
		{"ci", false},
		{"https://github.com/octo", true},
		{"https://github.com/octo/api/blob/main/README.md", true},
		{"http://example.com/octo/api/tree/main", true},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			assert.Equal(t, tt.want, LooksLikeRepo(tt.arg))
		})
	}
}

func TestSpecEqual(t *testing.T) {
	base := Spec{Host: "github.com", Owner: "Octo", Repo: "Api", Ref: "Main"}
	assert.True(t, base.Equal(Spec{Host: "GitHub.com", Owner: "octo", Repo: "api", Ref: "Main"}), "names compare case-insensitively")
	assert.False(t, base.Equal(Spec{Host: "github.com", Owner: "octo", Repo: "api", Ref: "main"}), "refs are case-sensitive")
	assert.False(t, base.Equal(Spec{Host: "other.ghe.com", Owner: "octo", Repo: "api", Ref: "Main"}))
}

func TestSpecString(t *testing.T) {
	assert.Equal(t, "octo/api", Spec{Owner: "octo", Repo: "api"}.String())
	assert.Equal(t, "h.ghe.com/octo/api@v1", Spec{Host: "h.ghe.com", Owner: "octo", Repo: "api", Ref: "v1"}.String())
}
