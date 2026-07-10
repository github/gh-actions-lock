package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateLocalActions_CompositeActionFiles verifies that --migrate-local-actions
// rewrites `./…` not just in workflow files but also inside in-repo composite
// action definitions (action.yml), including ones nested a few directories deep.
// A `./…` whose target does not resolve to an in-repo action file is left
// untouched — that is the equivalence guard that keeps `$/` a safe replacement.
func TestMigrateLocalActions_CompositeActionFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	write(".github/workflows/ci.yml",
		"name: CI\non: push\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./my-action\n")
	write("my-action/action.yml",
		"name: My Action\nruns:\n  using: composite\n  steps:\n    - uses: ./helper\n    - uses: ./nonexistent\n")
	write("helper/action.yml",
		"name: Helper\nruns:\n  using: composite\n  steps:\n    - uses: ./nested/deep\n")
	write("nested/deep/action.yml",
		"name: Deep\nruns:\n  using: composite\n  steps:\n    - run: echo deep\n      shell: bash\n")

	t.Chdir(dir)

	total, err := migrateLocalActions([]string{filepath.Join(".github", "workflows", "ci.yml")})
	require.NoError(t, err)

	// workflow ./my-action, composite ./helper, composite ./nested/deep = 3.
	assert.Equal(t, 3, total)

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err)
		return string(b)
	}
	assert.Contains(t, read(".github/workflows/ci.yml"), "uses: $/my-action")
	assert.Contains(t, read("my-action/action.yml"), "uses: $/helper")
	assert.Contains(t, read("helper/action.yml"), "uses: $/nested/deep")
	// Unresolvable local path stays put — nothing at repo-root/nonexistent.
	assert.Contains(t, read("my-action/action.yml"), "uses: ./nonexistent")
}
