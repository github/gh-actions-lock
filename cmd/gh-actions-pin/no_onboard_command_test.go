package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/cmd/gh-actions-pin/format"
	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRepoWorkflow writes a workflow body plus an explicit lockfile YAML to a
// scratch repo, chdir's into it, and returns the repo dir and workflow path.
// Unlike writeTempWorkflow it lets the caller supply a hand-built lockfile so
// the onboarding boundary (which deps a workflow already tracks) can be set
// precisely; pass lockYAML == "" to omit the lockfile entirely.
func writeRepoWorkflow(t *testing.T, body, lockYAML string) (dir, wfPath string) {
	t.Helper()
	dir = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfRel := filepath.Join(".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(filepath.Join(dir, wfRel), []byte(strings.TrimSpace(body)+"\n"), 0o600))
	if lockYAML != "" {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, ".github", "workflows", "actions.lock"),
			[]byte(lockYAML), 0o600))
	}
	t.Chdir(dir)
	return dir, filepath.ToSlash(wfRel)
}

// TestCheck_NoOnboard_RefusesNewAction proves that under --no-onboard a new
// action inside an already-tracked workflow is refused: it surfaces an
// onboarding-required finding (severity error), is NOT pinned, and the run
// exits errSilent with clean JSON on stdout. The already-tracked sibling
// (checkout) is untouched.
func TestCheck_NoOnboard_RefusesNewAction(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	// Only setup-go (the new dep) is resolved during diagnosis; checkout is
	// seeded from the lockfile and must not hit the network.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

	lockYAML := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v6:sha1-" + checkoutSHA + "':\n" +
		"    branch: 'main'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6:sha1-" + checkoutSHA + "'\n"

	_, wfPath := writeRepoWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`, lockYAML)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-onboard", "--json=valid,findings", wfPath,
	)
	require.ErrorIs(t, err, errSilent)

	require.True(t, strings.HasPrefix(strings.TrimSpace(stdout), "{"),
		"stdout must be pure JSON, got: %q", stdout)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	f := payload.Findings[0]
	assert.Equal(t, "onboarding-required", f.Category)
	assert.Equal(t, "error", f.Severity)
	assert.Equal(t, "high", f.Confidence)
	assert.Contains(t, f.Dependency, "setup-go")

	// The refused action must NOT have been written into the lockfile.
	assert.NotContains(t, readTempLockfilePins(t), "setup-go",
		"--no-onboard must not pin the refused action")
}

// TestCheck_NoOnboard_RefChangedStillRepins proves that under --no-onboard a
// ref bump inside an already-tracked workflow still re-pins: checkout is
// tracked at v5 in the lockfile but the workflow now uses v6, so it re-pins to
// v6 (a RefChanged, not an onboarding boundary) and the run exits 0.
// TestCheck_NoOnboard_LeavesRefChanged proves that --no-onboard does NOT
// refuse a ref bump inside an already-tracked workflow: checkout is tracked at
// v5.0.0 in the lockfile but the workflow now uses v6.0.0, so it stays a
// ref-changed finding (the onboarding gate only rewrites not-pinned findings).
// Read-only (--no-fix) keeps the assertion on classification; the re-pin write
// itself is covered by TestCheck_DefaultJSON_AutofixWrites.
func TestCheck_NoOnboard_LeavesRefChanged(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	// v6.0.0 is resolved during diagnosis; the stale v5.0.0 pin is seeded.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)

	lockYAML := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v5.0.0:sha1-" + oldSHA + "':\n" +
		"    branch: 'main'\n    commit: 'sha1-" + oldSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v5.0.0:sha1-" + oldSHA + "'\n"

	_, wfPath := writeRepoWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.0
`, lockYAML)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-onboard", "--no-fix", "--json=valid,findings", wfPath,
	)
	// A ref-changed finding is still blocking under --no-fix → errSilent.
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	// The gate must NOT have rewritten checkout to onboarding-required: a
	// tracked action's ref bump re-pins, it is not an onboarding boundary.
	// (A benign stale warning for the old v5.0.0 entry may also be present.)
	var refChanged *format.Finding
	for i := range payload.Findings {
		assert.NotEqual(t, "onboarding-required", payload.Findings[i].Category,
			"a tracked ref bump must never be refused as onboarding")
		if payload.Findings[i].Category == "ref-changed" {
			refChanged = &payload.Findings[i]
		}
	}
	require.NotNil(t, refChanged, "expected a ref-changed finding for the bumped action")
	assert.Contains(t, refChanged.Dependency, "checkout")
}

// TestCheck_NoOnboardNoNarrow_RefBumpRepinsAndWrites is the consumer's headline
// case: under combined --no-onboard --no-narrow, a tracked action whose ref was
// bumped in the workflow (v5.0.0 → v6.0.0) re-pins to the new SHA AND the
// lockfile is written to disk. This exercises the verified-entry write path
// that previously failed with "branch is required" (the carried v5.0.0 entry
// arrives branchless from the read path; Set must preserve metadata for
// unchanged pins and still write the changed one).
func TestCheck_NoOnboardNoNarrow_RefBumpRepinsAndWrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	mainSHA := "2222222222222222222222222222222222222222"

	reg.Register(httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{"data": map[string]any{
			"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML)}}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{"ref": "refs/heads/main",
			"object": map[string]any{"sha": mainSHA, "type": "commit"}}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/compare/`),
		httpmock.JSONResponse(map[string]any{"status": "identical",
			"merge_base_commit": map[string]any{"sha": newSHA}}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]map[string]any{{"name": "v6.0.0", "commit": map[string]any{"sha": newSHA}}}))
	reg.Register(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main", "id": 1, "owner": map[string]any{"id": 1}}))

	lockYAML := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v5.0.0:sha1-" + oldSHA + "':\n" +
		"    branch: 'main'\n    commit: 'sha1-" + oldSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v5.0.0:sha1-" + oldSHA + "'\n"

	_, wfPath := writeRepoWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6.0.0
`, lockYAML)

	// Fix mode (no --no-fix): the bump must re-pin and write.
	_, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-onboard", "--no-narrow", "--json=valid,findings", wfPath,
	)
	// Auto-fixed: a clean re-pin run exits 0 (no surviving blocking findings).
	require.NoError(t, err)

	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "checkout@v6.0.0",
		"the bumped action must be re-pinned at the new ref")
	assert.Contains(t, pins, newSHA, "the lockfile must record the new SHA")
	// The stale v5.0.0 entry must be pruned: a re-pin converges so the lock
	// matches the workflow's uses: exactly (no dead pins accumulate).
	assert.NotContains(t, pins, "v5.0.0",
		"the stale ref must be pruned after the re-pin write")
	assert.NotContains(t, pins, oldSHA,
		"the orphaned dependency entry must be GC'd once nothing references it")
}

// TestCheck_NoOnboard_RefusesNewWorkflow proves that under --no-onboard a
// brand-new workflow (no lockfile entry at all) is refused entirely: every
// uses: ref becomes an onboarding-required finding, nothing is pinned, and the
// run exits errSilent.
func TestCheck_NoOnboard_RefusesNewWorkflow(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	// The new workflow's dep is resolved during diagnosis even though it will
	// be refused for pinning.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", checkoutSHA, nodeActionYAML),
			},
		}),
	)

	_, wfPath := writeRepoWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`, "")

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-onboard", "--json=valid,findings", wfPath,
	)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "onboarding-required", payload.Findings[0].Category)
	assert.Contains(t, payload.Findings[0].Dependency, "checkout")

	// No lockfile pin should have been created for the refused workflow.
	if b, rerr := os.ReadFile(filepath.Join(".github", "workflows", "actions.lock")); rerr == nil {
		assert.NotContains(t, string(b), "sha1-"+checkoutSHA,
			"--no-onboard must not pin a brand-new workflow")
	}
}
