package workflowfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAndExtractActionRefs(t *testing.T) {
	f, err := Load("testdata/simple.yml")
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	refs, localPaths, warnings := scan.Refs, scan.LocalPaths, scan.Warnings
	assert.Len(t, refs, 2)
	assert.Empty(t, localPaths)
	assert.Empty(t, warnings)

	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "v4", refs[0].Ref)
	assert.Equal(t, "actions/setup-go", refs[1].NWO())
	assert.Equal(t, "v5", refs[1].Ref)
}

func TestExtractActionRefsMixed(t *testing.T) {
	f, err := Load("testdata/mixed_refs.yml")
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	refs, localPaths, warnings := scan.Refs, scan.LocalPaths, scan.Warnings
	assert.Len(t, refs, 3)
	assert.Equal(t, "actions/checkout", refs[0].NWO())
	assert.Equal(t, "actions/cache", refs[1].NWO())
	assert.Equal(t, "save", refs[1].Path)
	assert.Equal(t, "actions/setup-node", refs[2].NWO())

	assert.Len(t, localPaths, 1)
	assert.Equal(t, "./local-action", localPaths[0])

	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "unparseable uses:")
}

func TestExtractActionRefs_SelfRepositoryClassification(t *testing.T) {
	content := []byte(`
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: $/actions/foo
      - uses: $/actions/foo
      - uses: ./local-action
  call:
    uses: $/.github/workflows/reusable.yml
  bad:
    uses: $/actions/foo@v1
`)
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()

	// Remote ref parsed normally.
	require.Len(t, scan.Refs, 1)
	assert.Equal(t, "actions/checkout", scan.Refs[0].NWO())

	// `./` still collected as a local path.
	assert.Equal(t, []string{"./local-action"}, scan.LocalPaths)

	// Bare `$/…` valid at both step and job level, deduplicated.
	assert.ElementsMatch(t,
		[]string{"$/actions/foo", "$/.github/workflows/reusable.yml"},
		scan.SelfRepositoryRefs,
	)

	// `$/…@ref` is the invalid form.
	assert.Equal(t, []string{"$/actions/foo@v1"}, scan.SelfRepositoryRefErrs)
}

func TestExtractActionRefs_SelfRepositoryOnly(t *testing.T) {
	content := []byte(`
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/actions/foo
`)
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	scan := f.ExtractActionRefs()
	assert.Empty(t, scan.Refs)
	assert.Empty(t, scan.LocalPaths)
	assert.Equal(t, []string{"$/actions/foo"}, scan.SelfRepositoryRefs)
	assert.Empty(t, scan.SelfRepositoryRefErrs)
}

func TestMigrateLocalActionsToSelfRepository(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github", "workflows"), 0o755))

	// An in-repo composite action exists → eligible for migration.
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, "local-action"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "local-action", "action.yml"),
		[]byte("runs:\n  using: composite\n"), 0o644))

	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	content := []byte("jobs:\n  build:\n    steps:\n" +
		"      - uses: ./local-action\n" +
		"      - uses: ./missing-action\n")
	require.NoError(t, os.WriteFile(workflowPath, content, 0o644))

	f, err := Load(workflowPath)
	require.NoError(t, err)

	out, changed, err := f.MigrateLocalActionsToSelfRepository()
	require.NoError(t, err)

	// Only the path with an in-repo action file is rewritten.
	assert.Equal(t, 1, changed)
	assert.Contains(t, string(out), "uses: $/local-action")
	assert.Contains(t, string(out), "uses: ./missing-action")
}

func TestMigrateLocalActionsToSelfRepository_NoLocalPaths(t *testing.T) {
	content := []byte("jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n")
	f, err := Parse("ci.yml", content)
	require.NoError(t, err)

	out, changed, err := f.MigrateLocalActionsToSelfRepository()
	require.NoError(t, err)
	assert.Equal(t, 0, changed)
	assert.Equal(t, content, out)
}

func TestDiscoverCompositeActionFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("runs:\n  using: composite\n"), 0o644))
	}
	write("action.yml")
	write("nested/deep/action.yaml")
	write(".github/actions/thing/action.yml")
	write("not-an-action.yml")
	// A stray file inside .git must be skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "action.yml"), []byte("x"), 0o644))

	got, err := DiscoverCompositeActionFiles(root)
	require.NoError(t, err)

	want := []string{
		filepath.Join(root, ".github/actions/thing/action.yml"),
		filepath.Join(root, "action.yml"),
		filepath.Join(root, "nested/deep/action.yaml"),
	}
	assert.Equal(t, want, got)
}

func TestDiscoverCompositeActionFiles_EmptyRoot(t *testing.T) {
	got, err := DiscoverCompositeActionFiles("")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDiscoverWorkflowsIn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ci.yml"), []byte("name: ci\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte("name: deploy\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# ignore\n"), 0o644))

	paths, err := DiscoverWorkflowsIn(dir)
	require.NoError(t, err)
	assert.Len(t, paths, 2)
	assert.Equal(t, filepath.Join(dir, "ci.yml"), paths[0])
	assert.Equal(t, filepath.Join(dir, "deploy.yaml"), paths[1])
}

func TestDiscoverWorkflowsIn_MissingDir(t *testing.T) {
	paths, err := DiscoverWorkflowsIn(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Nil(t, paths)
}

func TestExtractLocalCompositeRefs_RejectsPathTraversal(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".github", "workflows"), 0o755))
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	require.NoError(t, os.WriteFile(workflowPath, []byte("name: ci\n"), 0o644))

	_, warnings := ExtractLocalCompositeRefs(workflowPath, []string{"./../../etc"})

	var sawRefusal bool
	for _, w := range warnings {
		if strings.Contains(w, "refusing to read action file outside repo root") {
			sawRefusal = true
		}
	}
	assert.True(t, sawRefusal, "expected refusal warning, got: %#v", warnings)
}
