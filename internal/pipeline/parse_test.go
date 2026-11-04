package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllExpandsSelfRepositoryActionDependencies(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))

	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	require.NoError(t, os.WriteFile(workflowPath, []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: $/actions/root
  call:
    uses: $/.github/workflows/reusable.yml
`), 0o644))

	actionPath := filepath.Join(repoRoot, "actions", "root", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
	require.NoError(t, os.WriteFile(actionPath, []byte(`runs:
  using: composite
  steps:
    - uses: actions/checkout@v4
`), 0o644))

	parsed := ParseAll([]string{workflowPath}, nil)

	require.Len(t, parsed, 1)
	pw := parsed[0]
	require.Len(t, pw.Refs, 1)
	assert.Equal(t, "actions/checkout", pw.Refs[0].NWO())
	assert.Equal(t, "v4", pw.Refs[0].Ref)
	assert.NotEmpty(t, pw.RewriteRefs, "refs inside a self repository action are rewritable in their action file")
	assert.Equal(t, []string{actionPath}, pw.SelfActionFiles)
	assert.ElementsMatch(t, []string{"$/actions/root", "$/.github/workflows/reusable.yml"}, pw.SelfRepositoryRefs)
	assert.Empty(t, pw.SelfRepositoryResolutionErrs)
}

func TestRewritableRefs(t *testing.T) {
	const sha = "bcd2ba49218906704ab6c1aa796996da409d3eb1"
	ref := func(s string) parserlock.ActionRef { return *parserlock.ParseActionRef(s) }
	tests := []struct {
		name     string
		refs     []string
		selfRefs []string
		want     []string
	}{
		{
			name:     "SHA in a self repository action is kept everywhere",
			refs:     []string{"Octo/Cache@" + sha, "octo/cache/restore@" + sha, "actions/checkout@v4"},
			selfRefs: []string{"octo/cache/restore@" + sha},
			want:     []string{"actions/checkout@v4"},
		},
		{
			name:     "tag in a self repository action stays rewritable",
			refs:     []string{"octo/cache@v2"},
			selfRefs: []string{"octo/cache@v2"},
			want:     []string{"octo/cache@v2"},
		},
		{
			name: "SHA only in the workflow stays rewritable",
			refs: []string{"octo/cache@" + sha},
			want: []string{"octo/cache@" + sha},
		},
		{
			name:     "same repository at another commit stays rewritable",
			refs:     []string{"octo/cache@" + sha, "octo/cache@v2"},
			selfRefs: []string{"octo/cache@" + sha},
			want:     []string{"octo/cache@v2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var refs, selfRefs []parserlock.ActionRef
			for _, s := range tt.refs {
				refs = append(refs, ref(s))
			}
			for _, s := range tt.selfRefs {
				selfRefs = append(selfRefs, ref(s))
			}
			got := []string{}
			for _, r := range rewritableRefs(refs, selfRefs) {
				got = append(got, r.FullName()+"@"+r.Ref)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRewritableRefsIsNonNilWhenEmpty(t *testing.T) {
	const sha = "bcd2ba49218906704ab6c1aa796996da409d3eb1"
	refs := []parserlock.ActionRef{*parserlock.ParseActionRef("octo/cache@" + sha)}
	got := rewritableRefs(refs, refs)
	assert.NotNil(t, got, "a nil RewriteRefs means every ref is rewritable")
	assert.Empty(t, got)
}
