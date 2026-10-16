package pipeline

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllFrom(t *testing.T) {
	src := mapSource{
		".github/workflows/ci.yml": `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: $/actions/root
`,
		"actions/root/action.yml": `runs:
  using: composite
  steps:
    - uses: actions/setup-go@v5
`,
	}

	parsed := ParseAllFrom(context.Background(), src, []string{".github/workflows/ci.yml", ".github/workflows/missing.yml"}, nil)

	require.Len(t, parsed, 2)
	pw := parsed[0]
	require.NoError(t, pw.LoadErr)
	require.Len(t, pw.Refs, 2)
	assert.ElementsMatch(t, []string{"actions/checkout", "actions/setup-go"}, []string{pw.Refs[0].NWO(), pw.Refs[1].NWO()})
	assert.Equal(t, []string{"actions/root/action.yml"}, pw.SelfActionFiles)
	assert.Empty(t, pw.SelfRepositoryResolutionErrs)

	require.Error(t, parsed[1].LoadErr, "a missing workflow is a load error, not a skip")
	assert.ErrorIs(t, parsed[1].LoadErr, fs.ErrNotExist)
}

type mapSource map[string]string

func (m mapSource) ReadFile(_ context.Context, name string) ([]byte, error) {
	if s, ok := m[name]; ok {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

func (m mapSource) ListFiles(context.Context, string) ([]string, error) { return nil, nil }

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
