package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllExpandsSelfRepositoryDependencies(t *testing.T) {
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
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workflowPath), "reusable.yml"), []byte(`on:
  workflow_call:
jobs:
  nested:
    uses: $/.github/workflows/nested.yml
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workflowPath), "nested.yml"), []byte(`on:
  workflow_call:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5
`), 0o644))

	parsed := ParseAll([]string{workflowPath}, nil)

	require.Len(t, parsed, 1)
	pw := parsed[0]
	require.Len(t, pw.Refs, 2)
	assert.Equal(t, "actions/checkout", pw.Refs[0].NWO())
	assert.Equal(t, "v4", pw.Refs[0].Ref)
	assert.Equal(t, "actions/setup-go", pw.Refs[1].NWO())
	assert.Equal(t, "v5", pw.Refs[1].Ref)
	assert.NotNil(t, pw.RewriteRefs)
	assert.Empty(t, pw.RewriteRefs, "a ref shared with a self repository action must not be rewritten in only the workflow")
	assert.ElementsMatch(t, []string{
		"$/actions/root",
		"$/.github/workflows/reusable.yml",
		"$/.github/workflows/nested.yml",
	}, pw.SelfRepositoryRefs)
	assert.Empty(t, pw.SelfRepositoryResolutionErrs)
}
