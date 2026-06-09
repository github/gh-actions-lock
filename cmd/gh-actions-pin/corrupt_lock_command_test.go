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
