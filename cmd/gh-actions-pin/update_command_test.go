package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateJSON is the decoded shape of `update --json` output. It mirrors the
// Dependabot consumer contract: three always-on arrays plus the verdict.
type updateJSON struct {
	CLIVersion      string `json:"cli_version"`
	LockfileVersion string `json:"lockfile_version"`
	Valid           bool   `json:"valid"`
	Updated         []struct {
		NWO    string `json:"nwo"`
		OldRef string `json:"old_ref"`
		NewRef string `json:"new_ref"`
		OldSHA string `json:"old_sha"`
		NewSHA string `json:"new_sha"`
	} `json:"updated"`
	Workflows []struct {
		Path string `json:"path"`
	} `json:"workflows"`
	Findings []struct {
		Workflow string `json:"workflow"`
		Category string `json:"category"`
		Severity string `json:"severity"`
	} `json:"findings"`
}

// registerBranchDiscovery stubs the REST calls DiscoverContaining makes for a
// freshly bumped SHA: repo metadata (default branch + IDs), the default-branch
// HEAD, the ancestry compare, and the tag list.
func registerBranchDiscovery(reg *httpmock.Registry, owner, repo, sha, tag string) {
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             2,
			"owner":          map[string]any{"id": 1},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/git/ref/heads/main`),
		httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]any{"sha": "9999999999999999999999999999999999999999", "type": "commit"},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/compare/`),
		httpmock.JSONResponse(map[string]any{
			"status":            "identical",
			"merge_base_commit": map[string]any{"sha": sha},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": tag, "commit": map[string]any{"sha": sha}},
		}),
	)
}

func TestUpdateCommand_CleanRelock(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "11111111111111111111111111111111111111aa"
	)

	// Resolve checkout@v6 → newSHA (nodeActionYAML has no transitives).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerBranchDiscovery(reg, "actions", "checkout", newSHA, "v6")

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.NoError(t, err, "clean relock should exit 0")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	assert.True(t, got.Valid)
	require.Len(t, got.Updated, 1)
	assert.Equal(t, "actions/checkout", got.Updated[0].NWO)
	assert.Equal(t, "v4", got.Updated[0].OldRef)
	assert.Equal(t, "v6", got.Updated[0].NewRef)
	assert.Equal(t, oldSHA, got.Updated[0].OldSHA)
	assert.Equal(t, newSHA, got.Updated[0].NewSHA)

	require.Len(t, got.Workflows, 1)
	assert.Equal(t, ".github/workflows/workflow.yml", got.Workflows[0].Path)
	assert.Empty(t, got.Findings)

	// Working tree mutated: YAML ref bumped, lockfile pin points at newSHA.
	body, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "actions/checkout@v6")
	assert.NotContains(t, string(body), "actions/checkout@v4")

	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/checkout@v6:sha1-"+newSHA)
	assert.NotContains(t, pins, oldSHA)
}

func TestUpdateCommand_OnboardingPartialSuccess(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "22222222222222222222222222222222222222bb"
	)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerBranchDiscovery(reg, "actions", "checkout", newSHA, "v6")

	// workflow.yml is onboarded (in the lockfile).
	onboarded := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)

	// workflow2.yml uses the target too but has NO lockfile entry.
	unonboarded := filepath.Join(filepath.Dir(onboarded), "workflow2.yml")
	require.NoError(t, os.WriteFile(unonboarded, []byte(strings.TrimSpace(`
name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`)+"\n"), 0o600))

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		onboarded, filepath.ToSlash(filepath.Join(".github", "workflows", "workflow2.yml")),
	)
	require.ErrorIs(t, err, errSilent, "onboarding-required is a blocking finding → exit 1")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be parseable JSON despite exit 1")

	assert.False(t, got.Valid)

	// The onboarded workflow was updated and saved.
	require.Len(t, got.Updated, 1)
	assert.Equal(t, "actions/checkout", got.Updated[0].NWO)
	require.Len(t, got.Workflows, 1)
	assert.Equal(t, ".github/workflows/workflow.yml", got.Workflows[0].Path)

	// The un-onboarded workflow produced an onboarding-required finding.
	require.Len(t, got.Findings, 1)
	assert.Equal(t, "onboarding-required", got.Findings[0].Category)
	assert.Equal(t, "error", got.Findings[0].Severity)
	assert.Equal(t, ".github/workflows/workflow2.yml", got.Findings[0].Workflow)

	// The un-onboarded workflow file was NOT mutated.
	body, err := os.ReadFile(unonboarded)
	require.NoError(t, err)
	assert.Contains(t, string(body), "actions/checkout@v4", "un-onboarded workflow must be left untouched")
}

func TestUpdateCommand_NoOp(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Already pinned at v6 → newSHA. SeedFromLockfile serves the resolve from
	// cache (no GraphQL stub) and the (NWO, SHA) reuse skips branch discovery,
	// so a true no-op makes zero HTTP calls.
	const sha = "33333333333333333333333333333333333333cc"

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6:sha1-"+sha,
	)
	before, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore := readTempLockfilePins(t)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.NoError(t, err, "no-op should exit 0")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	// Contract: the three arrays + valid + versions are ALWAYS present, even
	// on a no-op, and updated/findings/workflows are JSON arrays (never null).
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))
	for _, key := range []string{"cli_version", "lockfile_version", "valid", "updated", "findings", "workflows"} {
		_, ok := raw[key]
		assert.Truef(t, ok, "top-level key %q must always be present", key)
	}
	for _, key := range []string{"updated", "findings", "workflows"} {
		assert.Truef(t, strings.HasPrefix(strings.TrimSpace(string(raw[key])), "["), "%q must be a JSON array, got %s", key, raw[key])
	}

	assert.True(t, got.Valid)
	assert.Empty(t, got.Workflows)
	assert.Empty(t, got.Findings)

	after, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "no-op must not touch the workflow")
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "no-op must not touch the lockfile")
}

func TestUpdateCommand_DryRunWritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "44444444444444444444444444444444444444dd"
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerBranchDiscovery(reg, "actions", "checkout", newSHA, "v6")

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)
	before, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore := readTempLockfilePins(t)

	// No --write: the diff is computed but nothing is written.
	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.NoError(t, err)

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	require.Len(t, got.Updated, 1, "dry run still reports the diff")
	assert.Empty(t, got.Workflows, "dry run saves no workflows")

	after, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "dry run must not touch the workflow")
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "dry run must not touch the lockfile")
}

func TestUpdateCommand_MissingActionFlag(t *testing.T) {
	reg := &httpmock.Registry{}

	_, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--json=updated",
	)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errSilent, "a usage error is a tool failure (exit 2), not a findings exit")
}

func TestUpdateCommand_ImpostorTargetIsBlockingNotFailure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "55555555555555555555555555555555555555ee"
	)

	// Target ref resolves to a SHA that is on no branch (a fork-network /
	// tag-poisoning signal) → DiscoverContaining returns *ImpostorError.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	// Phase 1 (protected) and phase 2 (full listing) both empty → impostor.
	reg.Register(
		httpmock.RESTWithQuery("GET", `repos/actions/checkout/branches`, "protected=true"),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)
	before, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore := readTempLockfilePins(t)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.ErrorIs(t, err, errSilent, "impostor is a blocking finding (exit 1), not a tool failure (exit 2)")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must carry clean JSON")

	assert.False(t, got.Valid)
	assert.Empty(t, got.Updated, "an impostor target must not be reported as updated")
	assert.Empty(t, got.Workflows, "nothing is saved when the target is an impostor")
	require.Len(t, got.Findings, 1)
	assert.Equal(t, "impostor-commit", got.Findings[0].Category)
	assert.Equal(t, "error", got.Findings[0].Severity)

	// Working tree untouched.
	after, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.Equal(t, lockBefore, readTempLockfilePins(t))
}

func TestUpdateCommand_MixedCaseYAMLMatch(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "66666666666666666666666666666666666666ff"
	)

	// Resolver canonicalizes NWO to lowercase via the response's nameWithOwner.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerBranchDiscovery(reg, "actions", "checkout", newSHA, "v6")

	// YAML uses non-canonical casing; the lockfile key is canonical lowercase.
	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: Actions/Checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.NoError(t, err)

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	require.Len(t, got.Updated, 1)
	assert.Equal(t, "actions/checkout", got.Updated[0].NWO, "JSON nwo must be canonical")
	require.Len(t, got.Workflows, 1)

	// YAML rewrite preserves the author's casing; lockfile uses canonical key.
	body, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Actions/Checkout@v6", "rewrite must preserve YAML casing")
	assert.NotContains(t, string(body), "Actions/Checkout@v4")

	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/checkout@v6:sha1-"+newSHA, "lockfile key must be canonical")
	assert.NotContains(t, pins, oldSHA)
}

func TestUpdateCommand_BumpPullsInNewTransitive(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA   = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA   = "11111111111111111111111111111111111111aa"
		transSHA = "abababababababababababababababababababab"
	)

	// checkout@v6 is now a composite that fans out to a brand-new transitive.
	compositeYAML := "name: Checkout\n" +
		"runs:\n" +
		"  using: composite\n" +
		"  steps:\n" +
		"    - uses: vendor/dep@v2\n"

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, compositeYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("vendor", "dep"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("vendor/dep", transSHA, nodeActionYAML),
			},
		}),
	)
	registerBranchDiscovery(reg, "actions", "checkout", newSHA, "v6")
	registerBranchDiscovery(reg, "vendor", "dep", transSHA, "v2")

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-"+oldSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v6", "--write", "--no-onboard", "--json=updated",
		wfPath,
	)
	require.NoError(t, err)

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))

	assert.True(t, got.Valid)
	// The transitive add does NOT create a second updated[] entry — only the
	// targeted action is reported as changed.
	require.Len(t, got.Updated, 1)
	assert.Equal(t, "actions/checkout", got.Updated[0].NWO)
	require.Len(t, got.Workflows, 1)
	assert.Empty(t, got.Findings)

	// The new transitive landed in the lockfile alongside the bumped target.
	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/checkout@v6:sha1-"+newSHA)
	assert.Contains(t, pins, "vendor/dep@v2:sha1-"+transSHA, "new transitive must be written to the lockfile")
	assert.NotContains(t, pins, oldSHA)
}
