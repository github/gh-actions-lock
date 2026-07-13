package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeStaleLockfileRepo builds a scratch repo whose lockfile records two
// workflows — workflow.yml (present on disk, uses checkout@v6) and deleted.yml
// (no file on disk, uses setup-go@v6) — and chdirs into it. Both pins are
// mutable v6 refs so they're trusted from the lockfile without any network
// call. Returns the lockfile path.
func writeStaleLockfileRepo(t *testing.T) string {
	t.Helper()
	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "workflow.yml"), []byte(wfBody), 0o600))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/setup-go@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + setupGoSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v6'\n" +
		"  '.github/workflows/deleted.yml':\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

// TestCheck_FullScan_PrunesDeletedWorkflow proves a default full-directory fix
// run drops the lockfile entry for a workflow that no longer exists on disk,
// and that the now-orphaned dependency is garbage-collected with it.
func TestCheck_FullScan_PrunesDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)

	// No explicit workflow-path arg → full scan → prune authority.
	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.NoError(t, err)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	got := string(lockAfter)
	assert.Contains(t, got, "workflow.yml", "live workflow must be retained")
	assert.Contains(t, got, "actions/checkout", "live dep must be retained")
	assert.NotContains(t, got, "deleted.yml", "stale workflow entry must be pruned")
	assert.NotContains(t, got, "setup-go", "orphaned dep must be garbage-collected")
}

// TestCheck_PartialInvocation_DoesNotPrune proves an explicit-path invocation
// has no authority to declare another workflow deleted: the stale entry and its
// dep survive untouched.
func TestCheck_PartialInvocation_DoesNotPrune(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows", ".github/workflows/workflow.yml")
	require.NoError(t, err)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	got := string(lockAfter)
	assert.Contains(t, got, "deleted.yml", "partial run must not prune out-of-scope workflows")
	assert.Contains(t, got, "setup-go", "partial run must not GC an out-of-scope workflow's dep")
}

// TestCheck_NoFix_ReportsStaleWorkflow proves read-only mode surfaces a stale
// entry as a non-blocking info finding without touching the lockfile.
func TestCheck_NoFix_ReportsStaleWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeStaleLockfileRepo(t)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings")
	require.NoError(t, err, "stale-workflow is non-blocking, exit stays 0")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)

	var found *format.Finding
	for i := range payload.Findings {
		if payload.Findings[i].Category == "stale-workflow" {
			found = &payload.Findings[i]
			break
		}
	}
	require.NotNil(t, found, "expected a stale-workflow finding: %+v", payload.Findings)
	assert.Equal(t, "info", found.Severity)
	assert.Contains(t, found.Detail, "deleted.yml")

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}
