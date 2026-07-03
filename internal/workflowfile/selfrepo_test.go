package workflowfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSelfRepoAction(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"$/actions/my-action", true},
		{"  $/actions/my-action  ", true},
		{"$/foo/bar@v1", true},
		{"./local-action", false},
		{"actions/checkout@v4", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsSelfRepoAction(tt.value), "IsSelfRepoAction(%q)", tt.value)
	}
}

func TestSelfRepoRefHasVersion(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"$/actions/my-action", false},
		{"$/actions/my-action@v1", true},
		{"$/a/b@main", true},
		{"./local@v1", false}, // not a self-repo ref
		{"actions/checkout@v4", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, SelfRepoRefHasVersion(tt.value), "SelfRepoRefHasVersion(%q)", tt.value)
	}
}

func TestExtractActionRefsSelfRepo(t *testing.T) {
	content := `name: selfrepo
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: $/actions/valid-action
      - uses: $/actions/bad-action@v1
      - uses: ./local-action
`
	f, err := Parse("wf.yml", []byte(content))
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	assert.Len(t, scan.Refs, 1)
	assert.Equal(t, "actions/checkout", scan.Refs[0].NWO())

	assert.Equal(t, []string{"$/actions/valid-action"}, scan.SelfRepoRefs)
	assert.Equal(t, []string{"$/actions/bad-action@v1"}, scan.SelfRepoRefErrs)
	assert.Equal(t, []string{"./local-action"}, scan.LocalPaths)
}

func TestExtractActionRefsSelfRepoDedup(t *testing.T) {
	content := `on: push
jobs:
  a:
    steps:
      - uses: $/actions/foo
  b:
    steps:
      - uses: $/actions/foo
`
	f, err := Parse("wf.yml", []byte(content))
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	assert.Equal(t, []string{"$/actions/foo"}, scan.SelfRepoRefs)
}

func TestMigrateLocalActionsToSelfRepo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))

	// Two composite action dirs, plus one referenced path with no action file.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "actions", "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "actions", "build", "action.yml"), []byte("runs:\n  using: composite\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "actions", "deploy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "actions", "deploy", "action.yaml"), []byte("runs:\n  using: composite\n"), 0o644))

	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	wfPath := filepath.Join(wfDir, "ci.yml")
	content := `name: ci
on: push
jobs:
  b:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4  # keep me
      - uses: ./actions/build
      - uses: ./actions/deploy
      - uses: ./actions/missing
`
	require.NoError(t, os.WriteFile(wfPath, []byte(content), 0o644))

	f, err := Load(wfPath)
	require.NoError(t, err)

	got, changed, err := f.MigrateLocalActionsToSelfRepo()
	require.NoError(t, err)
	assert.Equal(t, 2, changed)

	gotStr := string(got)
	assert.Contains(t, gotStr, "uses: $/actions/build")
	assert.Contains(t, gotStr, "uses: $/actions/deploy")
	// Path without an action file is left untouched.
	assert.Contains(t, gotStr, "uses: ./actions/missing")
	// Remote refs and trailing comments are preserved.
	assert.Contains(t, gotStr, "uses: actions/checkout@v4  # keep me")
}

func TestMigrateLocalActionsToSelfRepoNoLocals(t *testing.T) {
	content := `on: push
jobs:
  b:
    steps:
      - uses: actions/checkout@v4
`
	f, err := Parse("wf.yml", []byte(content))
	require.NoError(t, err)

	got, changed, err := f.MigrateLocalActionsToSelfRepo()
	require.NoError(t, err)
	assert.Equal(t, 0, changed)
	assert.Equal(t, content, string(got))
}
