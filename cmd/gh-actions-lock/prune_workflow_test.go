package main

import (
	"encoding/json"
	"net/http"
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

// writeLastWorkflowDeletedRepo builds a scratch repo whose lockfile records a
// single workflow that no longer exists on disk (the workflows directory holds
// only the lockfile). It chdirs into the repo and returns the lockfile path.
func writeLastWorkflowDeletedRepo(t *testing.T) string {
	t.Helper()
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))

	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/setup-go@v6':\n" +
		"    ref: 'v6'\n    commit: 'sha1-" + setupGoSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/deleted.yml':\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

// TestCheck_FullScan_PrunesLastDeletedWorkflow proves that deleting the final
// workflow doesn't strand its lockfile entry: a full scan that discovers zero
// workflows still loads the lockfile and prunes the stale entry (and its
// orphaned dep) instead of aborting with "no workflow files found".
func TestCheck_FullScan_PrunesLastDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeLastWorkflowDeletedRepo(t)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.NoError(t, err, "an empty scan with a stale lockfile must prune, not error")

	// Pruning the last workflow empties the lockfile; the tool cleans up the
	// now-empty file. Either way the stale entry must not survive.
	lockAfter, err := os.ReadFile(lockPath)
	if err == nil {
		got := string(lockAfter)
		assert.NotContains(t, got, "deleted.yml", "stale workflow entry must be pruned")
		assert.NotContains(t, got, "setup-go", "orphaned dep must be garbage-collected")
	} else {
		assert.True(t, os.IsNotExist(err), "unexpected error reading lockfile: %v", err)
	}
}

// TestCheck_NoFix_ReportsLastDeletedWorkflow proves read-only mode still
// surfaces the stale entry when the last workflow is gone, without touching the
// lockfile.
func TestCheck_NoFix_ReportsLastDeletedWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	lockPath := writeLastWorkflowDeletedRepo(t)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,findings")
	require.NoError(t, err, "stale-workflow is non-blocking, exit stays 0")

	var payload struct {
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	var found bool
	for i := range payload.Findings {
		if payload.Findings[i].Category == "stale-workflow" {
			found = true
			assert.Contains(t, payload.Findings[i].Detail, "deleted.yml")
		}
	}
	assert.True(t, found, "expected a stale-workflow finding: %+v", payload.Findings)

	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// TestCheck_EmptyRepo_NoLockfile_Errors proves the helpful "no workflow files"
// error is preserved when there's nothing on disk and nothing to prune.
func TestCheck_EmptyRepo_NoLockfile_Errors(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no workflow files found")
}

func TestProximaPrunesBeforeVerifyingHosts(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		last       bool
		renamed    bool
		transitive bool
		foreign    bool
		namesake   bool
		public     bool
		mismatch   bool
		wantError  bool
	}{
		{name: "inaccessible stale pin is removed"},
		{name: "last deleted workflow is removed", last: true},
		{name: "renamed workflow preserves its historical pin", renamed: true},
		{name: "stale foreign host does not bind the resolver", foreign: true},
		{name: "stale namesake cannot seed identity caches", namesake: true},
		{name: "stale namesake cannot bind a conflicting host", namesake: true, public: true},
		{name: "partial scan retains out of scope pins", args: []string{".github/workflows/workflow.yml"}, wantError: true},
		{name: "readonly retains stale pins", args: []string{"--no-fix"}, wantError: true},
		{name: "retained transitive pin is still verified", transitive: true, wantError: true},
		{name: "retained identity mismatch leaves disk untouched", mismatch: true, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.last {
				path = writeLastWorkflowDeletedRepo(t)
			} else {
				path = writeStaleLockfileRepo(t)
			}
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			before := string(raw)
			if tt.renamed {
				before = strings.Replace(before, ".github/workflows/workflow.yml", ".github/workflows/old.yml", 1)
			}
			if tt.transitive {
				before = strings.Replace(before, "  'actions/checkout@v6':\n",
					"  'actions/checkout@v6':\n    uses: ['actions/setup-go@v6']\n", 1)
			}
			if tt.foreign {
				before = strings.Replace(before, "  'actions/setup-go@v6':\n",
					"  'actions/setup-go@v6':\n    hostname: 'other.ghe.com'\n", 1)
			}
			if tt.namesake {
				before = strings.ReplaceAll(before, "actions/setup-go@v6", "actions/checkout@v5")
				before = strings.Replace(before, "  'actions/checkout@v5':\n    ref: 'v6'",
					"  'actions/checkout@v5':\n    ref: 'v5'", 1)
				if tt.public {
					before = strings.Replace(before, "  'actions/checkout@v5':\n",
						"  'actions/checkout@v5':\n    hostname: 'github.com'\n", 1)
				}
				before = strings.Replace(before, "sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c'\n    owner_id: 1\n    repo_id: 1",
					"sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c'\n    owner_id: 2\n    repo_id: 2", 1)
			}
			require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
			staleCalls := 0
			transport := proximaTransport(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "api.tenant.ghe.com", req.URL.Host)
				switch req.URL.Path {
				case "/repos/actions/checkout":
					id := 1
					if tt.mismatch {
						id = 2
					}
					return httpmock.JSONResponse(map[string]any{"id": id, "owner": map[string]any{"id": 1}})(req)
				case "/repos/actions/setup-go":
					staleCalls++
					return httpmock.StatusResponse(http.StatusNotFound)(req)
				case "/graphql":
					return parityOK(req)
				case "/repos/actions/checkout/tags", "/repos/actions/checkout/releases":
					return httpmock.JSONResponse([]any{})(req)
				default:
					t.Errorf("unexpected request: %s", req.URL)
					return httpmock.StatusResponse(http.StatusInternalServerError)(req)
				}
			})
			args := append([]string{"--hostname", "tenant.ghe.com", "--no-interactive", "--no-narrow",
				"--no-migrate-local-actions", "--json=valid,findings"}, tt.args...)
			stdout, _, err := runCommandWithHTTP(t, transport, args...)
			if tt.wantError {
				require.Error(t, err)
				after, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				assert.Equal(t, before, string(after))
				if tt.mismatch {
					assert.ErrorContains(t, err, "does not match its tenant.ghe.com repository IDs")
					assert.Zero(t, staleCalls)
				} else {
					assert.ErrorContains(t, err, "verifying repository identity")
					assert.Positive(t, staleCalls)
				}
				return
			}
			require.NoError(t, err)
			assert.Zero(t, staleCalls)
			assert.Contains(t, stdout, "stale-workflow")
			after, err := os.ReadFile(path)
			if tt.last {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, string(after), "actions/checkout@v6")
			assert.Contains(t, string(after), "repo_id: 1")
			assert.Contains(t, string(after), "sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")
			assert.NotContains(t, string(after), "deleted.yml")
			assert.NotContains(t, string(after), "old.yml")
			assert.NotContains(t, string(after), "actions/setup-go")
			assert.NotContains(t, string(after), "actions/checkout@v5")
			assert.NotContains(t, string(after), "repo_id: 2")
		})
	}
}
