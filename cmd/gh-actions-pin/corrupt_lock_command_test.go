package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const corruptLockBody = "version: 'v0.0.1'\ndependencies:\n" +
	"  'actions/checkout@v4:sha1-1111111111111111111111111111111111111111':\n" +
	"    branch: 'main'\n" + // owner_id/repo_id absent → whole-file parse reject
	"    commit: 'sha1-1111111111111111111111111111111111111111'\n" +
	"workflows:\n  '.github/workflows/workflow.yml':\n" +
	"    - 'actions/checkout@v4:sha1-1111111111111111111111111111111111111111'\n"

func writeCorruptLock(t *testing.T) string {
	t.Helper()
	p := filepath.Join(".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte(corruptLockBody), 0o600))
	return p
}

// A corrupt lockfile must fail the run loudly (exit 2 → non-nil error, empty
// stdout) instead of being silently treated as empty. The file is left on disk
// for the user to inspect or fix. Non-TTY test sessions are headless, so the
// recovery policy never prompts.
func TestCorruptLockfile_CheckNoFix_FailsCleanly(t *testing.T) {
	body := `
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
	_ = writeTempWorkflow(t, body)
	lockPath := writeCorruptLock(t)

	stdout, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "check", "--no-fix", "--json=valid", ".github/workflows/workflow.yml")

	require.Error(t, err, "corrupt lockfile must fail the run")
	assert.Contains(t, err.Error(), "unreadable")
	assert.Empty(t, strings.TrimSpace(stdout), "no JSON on stdout for a tool failure (exit 2)")
	assert.FileExists(t, lockPath, "corrupt lockfile must be left in place under --no-fix")
}

func TestCorruptLockfile_Update_FailsCleanly(t *testing.T) {
	body := `
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
	_ = writeTempWorkflow(t, body)
	lockPath := writeCorruptLock(t)

	stdout, _, err := runCommandWithHTTP(t, &httpmock.Registry{},
		"update", "--action", "actions/checkout@v4", "--target", "v5", "--json=updated")

	require.Error(t, err, "corrupt lockfile must fail the relock")
	assert.Contains(t, err.Error(), "unreadable")
	assert.Empty(t, strings.TrimSpace(stdout), "no JSON on stdout for a tool failure (exit 2)")
	assert.FileExists(t, lockPath, "update must not delete the lockfile")
}

// fakeConfirmer (defined in lockrecovery_test.go) drives the interactive
// delete-and-recreate recovery path without a TTY.

// withConfirmFactory overrides the package confirm-factory hook for the
// duration of a test and restores it on cleanup.
func withConfirmFactory(t *testing.T, c confirmer, canPrompt bool) {
	t.Helper()
	prev := confirmFactoryHook
	confirmFactoryHook = func() (confirmer, bool) { return c, canPrompt }
	t.Cleanup(func() { confirmFactoryHook = prev })
}

// When an interactive `check` run hits a corrupt lockfile and the user confirms
// the delete, recovery must remove the unreadable file AND fully re-pin the
// workflows, writing a fresh, valid lockfile (real resolved SHA + owner/repo
// IDs) — not leave an empty record with nothing on disk.
func TestCorruptLockfile_CheckFix_ConfirmDeletes_Recreates(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const liveSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	const mainSHA = "1111111111111111111111111111111111111111"

	// Resolve checkout@v4.0.0 → liveSHA. A full-semver ref skips tag-narrowing,
	// so the remaining stubs only cover reverse lookup + the lockfile write.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", liveSHA, nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]any{"sha": mainSHA, "type": "commit"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{
			"status":            "identical",
			"merge_base_commit": map[string]any{"sha": liveSHA},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v4.0.0", "commit": map[string]any{"sha": liveSHA}},
		}),
	)
	reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             2,
			"owner":          map[string]any{"id": 1},
		}),
	)

	body := `
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4.0.0
`
	workflowPath := writeTempWorkflow(t, body)
	lockPath := writeCorruptLock(t)

	fc := &fakeConfirmer{result: true}
	withConfirmFactory(t, fc, true)

	_, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--json=valid,findings", workflowPath)
	require.NoError(t, err, "recovery + re-pin should succeed cleanly")
	assert.True(t, fc.called, "user must be prompted")

	// check fix-mode JSON reports the PRE-fix diagnosis (valid:false, a
	// not-pinned finding for the just-deleted lock) and then autofixes, exiting
	// 0. The proof recovery worked is the recreated lockfile, not `valid`.
	require.FileExists(t, lockPath, "recovery must recreate the lockfile")
	recreated := readTempLockfilePins(t)
	assert.Contains(t, recreated, "actions/checkout@v4.0.0:sha1-"+liveSHA,
		"recreated lock must carry the freshly resolved SHA, not the corrupt fake")
	assert.NotContains(t, recreated, "sha1-1111111111111111111111111111111111111111",
		"corrupt fake SHA must not survive recovery")
	assert.Contains(t, recreated, "owner_id:", "writer must emit owner_id")
	assert.Contains(t, recreated, "repo_id:", "writer must emit repo_id")
}

// When the user declines the delete prompt, the run fails loudly and the
// unreadable lockfile is left untouched.
func TestCorruptLockfile_CheckFix_DeclineKeepsFile(t *testing.T) {
	body := `
name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
	workflowPath := writeTempWorkflow(t, body)
	lockPath := writeCorruptLock(t)
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	fc := &fakeConfirmer{result: false}
	withConfirmFactory(t, fc, true)

	stdout, _, err := runCommandWithHTTP(t, &httpmock.Registry{},
		"check", "--json=valid", workflowPath)
	require.Error(t, err, "declining recovery must fail the run")
	assert.Contains(t, err.Error(), "unreadable")
	assert.Empty(t, strings.TrimSpace(stdout))

	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "declined recovery must leave the lockfile byte-identical")
}
