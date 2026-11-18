package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateLocalActions_CompositeActionFiles verifies that migration
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

	total, _, err := migrateLocalActions([]string{filepath.Join(".github", "workflows", "ci.yml")})
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

	total, _, err := migrateLocalActions([]string{firstPath, secondPath})

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

	total, _, err := migrateLocalActions([]string{workflowPath})

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

	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
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

	_, _, err := runCommandWithHTTP(t, reg, "--migrate-local-actions")
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
	assert.Contains(t, read("nested/deep/action.yml"), "uses: actions/setup-go@v6.0.0",
		"remote refs inside $/ actions are narrowed in their action file")

	// Lockfile: pins the remote dep, records nothing for the self refs.
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "actions/setup-go", "remote dep should be pinned")
	assert.Contains(t, lock, "'actions/setup-go@v6.0.0':", "narrowed ref matches the rewritten action file")
	assert.Contains(t, lock, setupGoSHA)
	assert.NotContains(t, lock, "$/", "self refs are inherently pinned; no lockfile entry")
	assert.NotContains(t, lock, "my-action", "in-repo composite must not be pinned")
	assert.NotContains(t, lock, "helper")
}

// TestMigrateLocalActions_OptIn: --migrate-local-actions rewrites an in-repo
// `./.github/actions/foo` composite ref to `$/…` with no network; a plain fix
// run, --no-fix, and --no-migrate-local-actions leave the file untouched.
func TestMigrateLocalActions_OptIn(t *testing.T) {
	const workflow = `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/foo
`
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
		wf := filepath.Join(dir, ".github", "workflows", "ci.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(wf), 0o755))
		require.NoError(t, os.WriteFile(wf, []byte(workflow), 0o600))
		act := filepath.Join(dir, ".github", "actions", "foo", "action.yml")
		require.NoError(t, os.MkdirAll(filepath.Dir(act), 0o755))
		require.NoError(t, os.WriteFile(act,
			[]byte("name: Foo\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n"), 0o600))
		t.Chdir(dir)
		return wf
	}
	read := func(t *testing.T, path string) string {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(b)
	}

	t.Run("default fix run leaves ./ in place", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, wf)
		require.NoError(t, err)
		assert.Equal(t, workflow, read(t, wf))
	})

	t.Run("--migrate-local-actions rewrites to $/", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, stderr, err := runCommandWithHTTP(t, transport, "--migrate-local-actions", wf)
		require.NoError(t, err)
		got := read(t, wf)
		assert.Contains(t, got, "uses: $/.github/actions/foo")
		assert.NotContains(t, got, "uses: ./.github/actions/foo")
		assert.Zero(t, transport.calls.Load(), "self ref is inherently pinned; no network")
		assert.Contains(t, stderr, "Migrated 1 local action to `$/…`")
	})

	t.Run("--no-fix leaves file untouched", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, "--no-fix", "--migrate-local-actions", wf)
		// The workflow isn't onboarded, so the read-only run fails after
		// diagnosis; migration would already have run by then.
		require.ErrorIs(t, err, errSilent)
		assert.Equal(t, workflow, read(t, wf))
	})

	t.Run("--no-migrate-local-actions leaves ./ in place", func(t *testing.T) {
		transport := &requestCountingTransport{}
		wf := setup(t)
		_, _, err := runCommandWithHTTP(t, transport, "--no-migrate-local-actions", wf)
		require.NoError(t, err, "command must succeed so the assertion proves the opt-out, not an early failure")
		got := read(t, wf)
		assert.Contains(t, got, "uses: ./.github/actions/foo", "opt-out leaves the ./… ref in place")
		assert.NotContains(t, got, "$/", "opt-out means no migration to $/")
	})
}

func TestMigrateLocalActions_UnreadableWorkflowAborts(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	write := func(rel, content string) string {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		return full
	}
	write("local/action.yml", "runs:\n  using: composite\n")
	const good = "jobs:\n  build:\n    steps:\n      - uses: ./local\n"
	goodPath := write(".github/workflows/good.yml", good)
	lockedPath := write(".github/workflows/locked.yml", good)
	require.NoError(t, os.Chmod(lockedPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(lockedPath, 0o644) })
	if _, err := os.ReadFile(lockedPath); err == nil {
		t.Skip("file permissions not enforced for this user")
	}

	total, _, err := migrateLocalActions([]string{goodPath, lockedPath})

	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.Zero(t, total)
	got, readErr := os.ReadFile(goodPath)
	require.NoError(t, readErr)
	assert.Equal(t, good, string(got))
}

// TestMigrateLocalActions_UnparseableWorkflowIsAFinding: a workflow that
// won't parse doesn't abort migration; it's reported like a default run.
func TestMigrateLocalActions_UnparseableWorkflowIsAFinding(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	const good = "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/foo\n"
	write(".github/workflows/good.yml", good)
	write(".github/workflows/bad.yml", "name: [\n")
	write(".github/actions/foo/action.yml",
		"name: Foo\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n")
	t.Chdir(dir)

	stdout, _, err := runCommandWithHTTP(t, reg, "--migrate-local-actions", "--json=valid,findings")

	require.ErrorIs(t, err, errSilent, "the load error blocks the write like a default run")
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	var loadErrors []string
	for _, f := range payload.Findings {
		if strings.Contains(f.Detail, "failed to load workflow") {
			loadErrors = append(loadErrors, f.Workflow)
		}
	}
	assert.Equal(t, []string{filepath.FromSlash(".github/workflows/bad.yml")}, loadErrors)
	got, readErr := os.ReadFile(filepath.Join(dir, ".github", "workflows", "good.yml"))
	require.NoError(t, readErr)
	assert.Equal(t, good, string(got), "a blocked run rolls back the migration")
}
