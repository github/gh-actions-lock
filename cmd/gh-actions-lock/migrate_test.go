package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
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

func TestMigrateLocalActions_StructuralErrorPreventsAllWrites(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) string {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		return full
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	write("local/action.yml", "runs:\n  using: composite\n")
	firstPath := write(".github/workflows/first.yml",
		"jobs:\n  build:\n    steps:\n      - uses: ./local\n")
	secondPath := write(".github/workflows/second.yml",
		"jobs:\n  build:\n    steps:\n      - uses: $/bad@v1\n")
	firstBefore, err := os.ReadFile(firstPath)
	require.NoError(t, err)

	total, err := migrateLocalActions([]string{firstPath, secondPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid self repository reference")
	assert.Zero(t, total)
	firstAfter, readErr := os.ReadFile(firstPath)
	require.NoError(t, readErr)
	assert.Equal(t, firstBefore, firstAfter)
}

func TestMigrateLocalActions_RejectsActionFileSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte("jobs:\n  build:\n    steps:\n      - uses: ./escape\n")
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o644))

	outsidePath := filepath.Join(t.TempDir(), "action.yml")
	outside := []byte("runs:\n  using: composite\n  steps:\n    - uses: ./helper\n")
	require.NoError(t, os.WriteFile(outsidePath, outside, 0o644))
	escapeDir := filepath.Join(dir, "escape")
	require.NoError(t, os.MkdirAll(escapeDir, 0o755))
	if err := os.Symlink(outsidePath, filepath.Join(escapeDir, "action.yml")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}

	total, err := migrateLocalActions([]string{workflowPath})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside repository root")
	assert.Zero(t, total)
	gotWorkflow, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflow, gotWorkflow)
	gotOutside, readErr := os.ReadFile(outsidePath)
	require.NoError(t, readErr)
	assert.Equal(t, outside, gotOutside)
}

// TestMigrateLocalActions_EndToEnd runs the real command with
// --migrate-local-actions against a scratch repo whose local composite chain
// reaches a pinnable remote action, then asserts on BOTH sides of
// the mutation: the rewritten workflow/action files AND the generated
// lockfile. It proves the two behaviours compose correctly in one pass:
//   - `./…` same-repo refs are rewritten to `$/…` in the workflow and in the
//     in-repo composite action.yml files (including nested ones);
//   - `$/…` self refs are inherently pinned, so they never appear in the
//     lockfile, while the genuine remote dep (actions/setup-go) is pinned to
//     its resolved commit.
func TestMigrateLocalActions_EndToEnd(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"
	mainSHA := "1111111111111111111111111111111111111111"

	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]any{"sha": mainSHA, "type": "commit"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/compare/`),
		httpmock.JSONResponse(map[string]any{
			"status":            "identical",
			"merge_base_commit": map[string]any{"sha": setupGoSHA},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v6.0.0", "commit": map[string]any{"sha": setupGoSHA}},
		}),
	)
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), httpmock.JSONResponse(map[string]any{
		"default_branch": "main",
		"id":             2,
		"owner":          map[string]any{"id": 1},
	}))

	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))

	// The workflow only names the local composite. The remote dependency lives
	// three levels down the in-repo action chain.
	write(".github/workflows/workflow.yml", strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./my-action
`)+"\n")
	// In-repo composite chain, all same-repo local refs.
	write("my-action/action.yml", "name: My Action\nruns:\n  using: composite\n  steps:\n    - uses: ./helper\n")
	write("helper/action.yml", "name: Helper\nruns:\n  using: composite\n  steps:\n    - uses: ./nested/deep\n")
	write("nested/deep/action.yml", "name: Deep\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n")

	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, reg,
		"--migrate-local-actions", ".github/workflows/workflow.yml",
	)
	require.NoError(t, err)

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err)
		return string(b)
	}

	// Workflow: local ref migrated; the remote ref remains in the action file.
	wf := read(".github/workflows/workflow.yml")
	assert.Contains(t, wf, "uses: $/my-action")
	assert.NotContains(t, wf, "uses: ./my-action")
	assert.NotContains(t, wf, "actions/setup-go")

	// In-repo composite action files migrated, including the nested one.
	assert.Contains(t, read("my-action/action.yml"), "uses: $/helper")
	assert.Contains(t, read("helper/action.yml"), "uses: $/nested/deep")
	assert.Contains(t, read("nested/deep/action.yml"), "uses: actions/setup-go@v6")

	// Lockfile: pins the remote dep, records nothing for the self refs.
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "actions/setup-go", "remote dep should be pinned")
	assert.Contains(t, lock, "'actions/setup-go@v6':", "nested refs keep the author's ref instead of being narrowed as workflow source")
	assert.Contains(t, lock, setupGoSHA)
	assert.NotContains(t, lock, "$/", "self refs are inherently pinned; no lockfile entry")
	assert.NotContains(t, lock, "my-action", "in-repo composite must not be pinned")
	assert.NotContains(t, lock, "helper")
}
