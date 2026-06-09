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
	registerResolveNoTags(reg, owner, repo, sha)
	reg.Register(
		httpmock.REST("GET", `repos/`+owner+`/`+repo+`/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": tag, "commit": map[string]any{"sha": sha}},
		}),
	)
}

// registerResolveNoTags registers the repo-metadata, default-branch, and
// compare stubs a relock needs, but NOT the /tags stub. Interactive picker
// tests supply their own multi-tag /tags stub because discover.Candidates and
// the resolver share one cached tag listing — registering two would leave one
// unmatched.
func registerResolveNoTags(reg *httpmock.Registry, owner, repo, sha string) {
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
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
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
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
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
		"update", "--action", "actions/checkout@v6", "--target", "v6", "--json=updated",
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
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
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
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
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
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
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

func TestUpdateCommand_SHAShapedTargetIsBlocking(t *testing.T) {
	// A bare-SHA target ref would relock to a sha-as-ref instead of a
	// freshly-pinned human ref. update must refuse it structurally — before any
	// network resolution — with a blocking sha-as-ref finding, touching nothing.
	reg := &httpmock.Registry{}
	defer reg.Verify(t) // no HTTP expected: the guard short-circuits pre-resolve

	const (
		oldSHA    = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		targetSHA = "abcdef0123456789abcdef0123456789abcdef01"
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
		"update", "--action", "actions/checkout@v4", "--target", targetSHA, "--json=updated",
		wfPath,
	)
	require.ErrorIs(t, err, errSilent, "sha-shaped target is a blocking finding (exit 1), not a tool failure")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must carry clean JSON")

	assert.False(t, got.Valid)
	assert.Empty(t, got.Updated)
	assert.Empty(t, got.Workflows)
	require.Len(t, got.Findings, 1)
	assert.Equal(t, "sha-as-ref", got.Findings[0].Category)
	assert.Equal(t, "error", got.Findings[0].Severity)

	// Working tree untouched (no --write side effects).
	after, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
	assert.Equal(t, lockBefore, readTempLockfilePins(t))
}

func TestUpdateCommand_WriteFailureLeavesTreeConsistent(t *testing.T) {
	// Atomicity guarantee: if a workflow write fails mid-commit, neither the
	// workflow YAML nor the lockfile may advance. We force the failure by making
	// the workflow directory read-only so staging the rewritten YAML fails
	// before the lockfile is saved.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; cannot inject a write failure")
	}

	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "77777777777777777777777777777777777777cc"
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

	// Make the workflow dir read-only so staging the new YAML fails. The
	// lockfile lives in the same dir, so a successful save is impossible too —
	// but staging happens first, so the lockfile is never even attempted.
	wfDir := filepath.Dir(wfPath)
	require.NoError(t, os.Chmod(wfDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(wfDir, 0o755) })

	_, _, err = runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
		wfPath,
	)
	require.Error(t, err, "a write failure must surface as an error")
	assert.NotErrorIs(t, err, errSilent, "an IO failure is a tool failure (exit 2), not a findings exit")

	// Restore perms so we can read back and so TempDir cleanup succeeds.
	require.NoError(t, os.Chmod(wfDir, 0o755))

	after, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "workflow YAML must be untouched after a failed write")
	assert.Equal(t, lockBefore, readTempLockfilePins(t), "lockfile must not advance past a failed workflow write")

	// No leftover staging temp file.
	entries, err := os.ReadDir(wfDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".gh-actions-pin.tmp", "staging temp must be cleaned up on failure")
	}
}

func TestUpdateCommand_WritePreservesFileMode(t *testing.T) {
	// The temp+rename writer must not widen a workflow's permission bits. The
	// test harness writes workflows at 0600; after a relock the file must still
	// be 0600, not a fresh 0644.
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "88888888888888888888888888888888888888bb"
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
	require.NoError(t, os.Chmod(wfPath, 0o600))

	_, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v4", "--target", "v6", "--json=updated",
		wfPath,
	)
	require.NoError(t, err)

	info, err := os.Stat(wfPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "relock must preserve the workflow's mode")

	body, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "actions/checkout@v6", "the rewrite still happened")
}

// checkoutTagsResponse is the repos/tags REST payload for actions/checkout used
// by the interactive picker tests.
func checkoutTagsResponse() []map[string]any {
	tag := func(name, sha string) map[string]any {
		return map[string]any{"name": name, "commit": map[string]any{"sha": sha}}
	}
	return []map[string]any{
		tag("v4", "de0fac2e4500dabe0009e67214ff5f5447ce83dd"),
		tag("v5", "22222222222222222222222222222222222222bb"),
		tag("v6", "11111111111111111111111111111111111111aa"),
	}
}

// TestUpdateCommand_InteractivePick exercises the no-target interactive flow:
// with no --target, update lists newer versions and relocks to the picked ref.
func TestUpdateCommand_InteractivePick(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const (
		oldSHA = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
		newSHA = "11111111111111111111111111111111111111aa"
	)

	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/tags"),
		httpmock.JSONResponse(checkoutTagsResponse()),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerResolveNoTags(reg, "actions", "checkout", newSHA)

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

	// Candidates for current v4 are [v6, v5] (semver-desc, strictly greater);
	// index 0 picks v6.
	prompt := &fakePrompter{selectIdx: 0}
	stdout, _, err := runUpdateInteractive(t, reg, prompt,
		"update", "--action", "actions/checkout@v4", "--json=updated", wfPath,
	)
	require.NoError(t, err, "interactive relock should exit 0")

	require.NotEmpty(t, prompt.gotOpts, "picker must be shown options")
	assert.Contains(t, prompt.gotOpts[0], "v6", "newest candidate ranks first")

	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	require.Len(t, got.Updated, 1)
	assert.Equal(t, "v4", got.Updated[0].OldRef)
	assert.Equal(t, "v6", got.Updated[0].NewRef)

	body, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), "actions/checkout@v6")
	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/checkout@v6:sha1-"+newSHA)
}

// TestUpdateCommand_InteractiveStdoutPurity guards that the picker UI never
// leaks onto stdout: with --json, stdout must be exactly the update JSON.
func TestUpdateCommand_InteractiveStdoutPurity(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	const newSHA = "11111111111111111111111111111111111111aa"
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/tags"),
		httpmock.JSONResponse(checkoutTagsResponse()),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", newSHA, nodeActionYAML),
			},
		}),
	)
	registerResolveNoTags(reg, "actions", "checkout", newSHA)

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	prompt := &fakePrompter{selectIdx: 0}
	stdout, _, err := runUpdateInteractive(t, reg, prompt,
		"update", "--action", "actions/checkout@v4", "--json=updated", wfPath,
	)
	require.NoError(t, err)

	trimmed := strings.TrimSpace(stdout)
	assert.True(t, strings.HasPrefix(trimmed, "{"), "stdout must start with JSON, got: %q", stdout)
	var got updateJSON
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout must be pure, parseable JSON")
}

// TestUpdateCommand_InteractiveDefaultIsCorePick proves the picker's DEFAULT
// selection mirrors dependabot-core's precision-preserving pick rather than the
// numerically-highest tag. For a major-form current pin (@v4), the highest tag
// overall is v6.1.2 (so it heads the list), but core would land the highest
// major-FORM tag, v6 — so the default must be v6, not v6.1.2. The prompter errors
// right after capturing its arguments, isolating the default-selection behavior
// from the relock that would otherwise follow.
func TestUpdateCommand_InteractiveDefaultIsCorePick(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	tag := func(name, sha string) map[string]any {
		return map[string]any{"name": name, "commit": map[string]any{"sha": sha}}
	}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/tags"),
		httpmock.JSONResponse([]map[string]any{
			tag("v4", "de0fac2e4500dabe0009e67214ff5f5447ce83dd"),
			tag("v5", "22222222222222222222222222222222222222bb"),
			tag("v6", "11111111111111111111111111111111111111aa"),
			tag("v6.1.2", "33333333333333333333333333333333333333cc"),
		}),
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
		"actions/checkout@v4:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	// Error after the prompter records its arguments so no relock follows.
	prompt := &fakePrompter{err: assert.AnError}
	_, _, err := runUpdateInteractive(t, reg, prompt,
		"update", "--action", "actions/checkout@v4", "--json=updated", wfPath,
	)
	require.Error(t, err)

	require.NotEmpty(t, prompt.gotOpts, "picker must be shown options")
	assert.True(t, strings.HasPrefix(prompt.gotOpts[0], "v6.1.2 "),
		"highest semver heads the list, got %q", prompt.gotOpts[0])
	assert.True(t, strings.HasPrefix(prompt.gotDefault, "v6 "),
		"default must be the core precision pick v6, not %q", prompt.gotDefault)
}

// TestUpdateCommand_NoTargetNonInteractive verifies that omitting --target in a
// non-interactive session (no TTY, default prompter factory) is a tool failure
// (exit 2), never a hang or silent auto-pick.
func TestUpdateCommand_NoTargetNonInteractive(t *testing.T) {
	reg := &httpmock.Registry{}

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout@v4", "--json=updated", wfPath,
	)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errSilent, "missing --target non-interactively is exit 2, not a findings exit")
	assert.Empty(t, strings.TrimSpace(stdout), "no JSON on a usage failure")
}

// TestUpdateCommand_NoTargetNoInteractiveFlag verifies --no-interactive forces
// the same exit-2 usage failure even if a prompter is available.
func TestUpdateCommand_NoTargetNoInteractiveFlag(t *testing.T) {
	reg := &httpmock.Registry{}

	wfPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`,
		"actions/checkout@v4:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	prompt := &fakePrompter{selectIdx: 0}
	_, _, err := runUpdateInteractive(t, reg, prompt,
		"update", "--action", "actions/checkout@v4", "--no-interactive", "--json=updated", wfPath,
	)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errSilent, "--no-interactive without --target is exit 2")
	assert.Empty(t, prompt.gotOpts, "prompter must not be invoked under --no-interactive")
}

// TestUpdateCommand_MissingRefOnAction verifies --action without an @ref is a
// usage error (exit 2): the ref is the required precision anchor.
func TestUpdateCommand_MissingRefOnAction(t *testing.T) {
	reg := &httpmock.Registry{}

	_, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"update", "--action", "actions/checkout", "--target", "v6", "--json=updated",
	)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errSilent, "missing @ref is a tool failure, not a findings exit")
}
