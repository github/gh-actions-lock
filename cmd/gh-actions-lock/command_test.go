package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCommand_JSONWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "4a3601121dd01d1626a1e23e37211e3254c1c06c", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`,
		"actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		"actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c",
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

// reachableFunc returns a checkReachFn that reports all commits as reachable.
func reachableFunc() func(context.Context, string, string, string, string) (resolve.ReachabilityStatus, string) {
	return func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
		return resolve.Reachable, "ancestor of " + ref
	}
}

// unreachableFunc returns a checkReachFn that reports all commits as unreachable.
func unreachableFunc() func(context.Context, string, string, string, string) (resolve.ReachabilityStatus, string) {
	return func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
		return resolve.Unreachable, "commit is not an ancestor of " + ref
	}
}

// unknownReachFunc returns a checkReachFn that reports unknown (clone failure).
func unknownReachFunc() func(context.Context, string, string, string, string) (resolve.ReachabilityStatus, string) {
	return func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
		return resolve.ReachabilityUnknown, "clone failed"
	}
}

func testRepoResponse(nameWithOwner, oid, actionYAML string) map[string]any {
	return map[string]any{
		"nameWithOwner": nameWithOwner,
		"object": map[string]any{
			"oid": oid,
			"file": map[string]any{
				"object": map[string]any{
					"text": actionYAML,
				},
			},
			"fileYaml": nil,
		},
	}
}

// writeTempWorkflow writes a workflow YAML body to a scratch repo at
// .github/workflows/workflow.yml and (if pins are provided) materializes the
// detached lockfile at .github/workflows/actions.lock. The fixture body must
// NOT carry an embedded `dependencies:` block — pin metadata lives in the
// detached lockfile per the per-repo dependency-pinning architecture.
// Returns the relative workflow path; the test's working directory is
// chdir'd to the scratch repo.
func writeTempWorkflow(t *testing.T, body string, pins ...string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))

	body = strings.TrimSpace(body) + "\n"
	if strings.Contains(body, "\ndependencies:") {
		t.Fatalf("writeTempWorkflow: workflow body contains embedded `dependencies:` block; pass pins as the variadic argument instead")
	}

	wfRel := filepath.Join(".github", "workflows", "workflow.yml")
	wfPath := filepath.Join(dir, wfRel)
	require.NoError(t, os.WriteFile(wfPath, []byte(body), 0o600))

	if len(pins) > 0 {
		writeTempLockfile(t, dir, "workflow.yml", pins)
	}

	t.Chdir(dir)
	return filepath.ToSlash(wfRel)
}

// writeTempLockfile writes a v0.0.1 actions.lock fixture covering the given
// workflow file. Owner/repo IDs are stubbed; the read path doesn't validate
// them. All user-supplied scalars are single-quoted to mirror the
// production emitter (see internal/lockfile/store.go::marshalDeterministic).
func writeTempLockfile(t *testing.T, repoDir, wfName string, pinStrings []string) {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("version: 'v0.0.1'\ndependencies:\n")
	for _, pin := range pinStrings {
		sb.WriteString("  '" + pin + "':\n    branch: 'main'\n    commit: 'sha1-deadbeef'\n    owner_id: 1\n    repo_id: 1\n")
	}
	sb.WriteString("workflows:\n  '.github/workflows/" + wfName + "':\n")
	for _, pin := range pinStrings {
		sb.WriteString("    - '" + pin + "'\n")
	}
	p := filepath.Join(repoDir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(p, []byte(sb.String()), 0o600))
}

// readTempLockfilePins returns the raw pin strings from the actions.lock file
// in the current working directory. Useful for assertions in write/upgrade
// tests that previously inspected the workflow YAML directly.
func readTempLockfilePins(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	return string(b)
}

func runCommandWithHTTP(t *testing.T, rt http.RoundTripper, args ...string) (string, string, error) {
	return runCommandWithHTTPAndReach(t, rt, nil, args...)
}

func runCommandWithHTTPAndReach(t *testing.T, rt http.RoundTripper, reachFn func(context.Context, string, string, string, string) (resolve.ReachabilityStatus, string), args ...string) (string, string, error) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	newResolver := func(hostname string, pool *pinpool.Pool) (*resolve.Resolver, error) {
		var opts []resolve.Option
		if reachFn != nil {
			opts = append(opts, resolve.WithCheckReachabilityFunc(reachFn))
		}
		return resolve.New(hostname, pool, append(opts, resolve.WithTransport(rt))...)
	}

	cmd := newRootCmd(newResolver)
	cmd.SetOut(stdoutW)
	cmd.SetErr(stderrW)
	cmd.SetArgs(args)
	runErr := cmd.Execute()

	_ = stdoutW.Close()
	_ = stderrW.Close()

	stdoutBytes, readErr := io.ReadAll(stdoutR)
	require.NoError(t, readErr)
	stderrBytes, readErr := io.ReadAll(stderrR)
	require.NoError(t, readErr)

	return string(stdoutBytes), string(stderrBytes), runErr
}

// ==========================================================================
// Supply Chain Attack Reachability Tests
//
// These tests model real-world attacks where tag mutation or fork-network
// injection was used to compromise GitHub Actions. The reachability check
// should catch cases where a pinned SHA exists in the GitHub fork network
// but is NOT on the canonical repository's ref lineage.
//
// References:
//   - tj-actions/changed-files (CVE-2025-30066): tag v44 pointed to malicious commit from fork
//   - reviewdog/action-setup: tag mutation via compromised PAT
//   - xygeni/xygeni-action: C2 reverse shell backdoor via tag poisoning
//   - aquasecurity/trivy-action: scanner-to-stealer tag manipulation
// ==========================================================================

// TestCheck_TjActionsChangedFiles_TagMutationAttack models the March 2025
// tj-actions/changed-files attack (CVE-2025-30066) where attackers
// compromised a maintainer PAT and force-pushed tag v44 to a malicious
// commit. The malicious commit is NOT reachable from the legitimate tag.
// TestCheck_TamperedAndUnreachable verifies that when a pinned SHA differs
// from live resolution AND the old SHA is unreachable, both errors are reported.
func TestCheck_TamperedAndUnreachable(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, unreachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are invalid")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	// SHA changed but Compare API is not mocked → ancestry returns
	// Unknown, so the SHA mismatch surfaces as ancestry-unknown
	// rather than ref-moved or lockfile-forgery.
	assert.True(t, categories["ancestry-unknown"], "should detect SHA changed (ancestry inconclusive): %+v", payload.Findings)
	assert.True(t, categories["impostor-commit"], "should detect unreachable commit: %+v", payload.Findings)
}

// TestCheck_UnreachableOnly verifies that when a pinned SHA matches live
// resolution but is not reachable from the ref, an impostor-commit error is reported.
func TestCheck_UnreachableOnly(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "cccccccccccccccccccccccccccccccccccccccc"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", sha, nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+sha,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, unreachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are invalid")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	hasUnreachable := false
	for _, f := range payload.Findings {
		if f.Category == "impostor-commit" {
			hasUnreachable = true
		}
	}
	assert.True(t, hasUnreachable, "should detect unreachable commit: %+v", payload.Findings)
}

// TestCheck_ReachabilityUnknown verifies that when the reachability check
// cannot complete, validation passes with a warning.
func TestCheck_ReachabilityUnknown(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "dddddddddddddddddddddddddddddddddddddddd"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", sha, nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+sha,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, unknownReachFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "unknown reachability should not fail the check")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "valid should be true when reachability is unknown")

	// Reachability unknown surfaces as CategoryReachabilityUnknown +
	// SeverityWarning so consumers (Dependabot FindingMapper) don't
	// see CategoryValid for a scan that didn't actually verify.
	hasWarning := false
	sawValid := false
	for _, f := range payload.Findings {
		if f.Severity == "warning" && strings.Contains(f.Detail, "clone failed") {
			hasWarning = true
			if f.Category != "reachability-unknown" {
				t.Errorf("category = %q, want %q (must not regress to valid+warning)", f.Category, "reachability-unknown")
			}
		}
		if f.Category == "valid" {
			sawValid = true
		}
	}
	assert.True(t, hasWarning, "should have a reachability warning: %+v", payload.Findings)
	assert.False(t, sawValid, "CategoryValid must not appear for an unverified scan: %+v", payload.Findings)
}

// TestCheck_Reachable verifies the happy path: pinned SHA matches live
// resolution and is reachable — validation passes with no errors or warnings.
func TestCheck_Reachable(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", sha, nodeActionYAML),
			},
		}),
	)
	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+sha,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

// ==========================================================================
// Lockfile Forgery Detection Tests
//
// These tests verify that the ancestry check promotes ref-moved to
// lockfile-forgery when the pinned SHA is not an ancestor of the live SHA.
// This detects cases where someone manually injected a SHA into the lockfile
// that was never part of the ref's legitimate history.
// ==========================================================================

// TestCheck_LockfileForgery_NotAncestor verifies that when the Compare API
// shows the pinned SHA is NOT an ancestor of the live SHA, the finding is
// promoted from ref-moved to lockfile-forgery.
func TestCheck_LockfileForgery_NotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// GraphQL resolution returns the live SHA (different from pinned).
	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: merge_base ≠ pinnedSHA → not an ancestor.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "diverged",
			"merge_base_commit": map[string]any{
				"sha": "cccccccccccccccccccccccccccccccccccccccc",
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero for forgery findings")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["lockfile-forgery"], "should detect lockfile forgery: %+v", payload.Findings)
	assert.False(t, categories["ref-moved"], "should NOT have ref-moved (promoted to forgery): %+v", payload.Findings)
}

// TestCheck_LockfileForgery_LegitAncestor verifies that when the Compare API
// confirms the pinned SHA IS an ancestor of the live SHA, the finding stays
// as ref-moved (legitimate tag movement, not forgery).
func TestCheck_LockfileForgery_LegitAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: merge_base == pinnedSHA → legitimate ancestor.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref-moved is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ref-moved"], "should keep as ref-moved for legit ancestor: %+v", payload.Findings)
	assert.False(t, categories["lockfile-forgery"], "should NOT have lockfile-forgery: %+v", payload.Findings)
}

// TestCheck_LockfileForgery_RateLimited verifies that when the ancestry
// check is rate-limited, the finding surfaces as ancestry-unknown — not
// ref-moved (which would imply a benign-but-known move) and not
// lockfile-forgery (which requires an authoritative not-ancestor answer).
func TestCheck_LockfileForgery_RateLimited(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	pinnedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: rate limited.
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.StatusResponse(429),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+pinnedSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ancestry-unknown is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ancestry-unknown"], "should classify as ancestry-unknown when rate limited: %+v", payload.Findings)
	assert.False(t, categories["lockfile-forgery"], "should NOT have lockfile-forgery when rate limited: %+v", payload.Findings)
	assert.False(t, categories["ref-moved"], "should NOT downgrade to ref-moved when rate limited: %+v", payload.Findings)
	assert.False(t, categories["valid"], "rate-limited ancestry must not regress to CategoryValid: %+v", payload.Findings)

	// Verify the detail mentions the inconclusive ancestry check.
	for _, f := range payload.Findings {
		if f.Category == "ancestry-unknown" {
			assert.Contains(t, f.Detail, "ancestry check inconclusive")
		}
	}
}

func TestCheckCommand_JSONDependenciesWithRequiredBy(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	// Per-ref resolution queries (parallel resolver resolves one ref per worker).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	// Transitive dep discovered from composite (cache/save).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
`,
		"actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		"actions/setup-go@v6:sha1-d35c59abb061a4a6fb18e82ac0862c26744d6ab5",
		// Transitive dependency (via actions/setup-go@v6).
		"actions/cache@v4:sha1-5a3ec84eff668545956fd18022155c47e93e2684",
	)

	// Test per-workflow dependencies view
	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Path         string              `json:"path"`
			Valid        bool                `json:"valid"`
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	wf := payload.Workflows[0]
	assert.True(t, wf.Valid)
	require.Len(t, wf.Dependencies, 3)

	// Find the transitive dep
	var transitiveDep *format.Dependency
	for i := range wf.Dependencies {
		if wf.Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &wf.Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)

	// Direct deps should NOT have required_by
	for _, d := range wf.Dependencies {
		if d.Direct {
			assert.Empty(t, d.RequiredBy, "direct dep %s should not have required_by", d.NWO)
		}
	}
}

func TestCheckCommand_JSONDependenciesInfersRequiredByWithoutComments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "cache"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/cache", "5a3ec84eff668545956fd18022155c47e93e2684", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v6
`,
		"actions/setup-go@v6:sha1-d35c59abb061a4a6fb18e82ac0862c26744d6ab5",
		"actions/cache@v4:sha1-5a3ec84eff668545956fd18022155c47e93e2684",
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Dependencies []format.Dependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	var transitiveDep *format.Dependency
	for i := range payload.Workflows[0].Dependencies {
		if payload.Workflows[0].Dependencies[i].NWO == "actions/cache" {
			transitiveDep = &payload.Workflows[0].Dependencies[i]
			break
		}
	}
	require.NotNil(t, transitiveDep, "transitive dep actions/cache should be present")
	assert.False(t, transitiveDep.Direct)
	assert.Equal(t, []string{"actions/setup-go@v6"}, transitiveDep.RequiredBy)
}

func TestCheckCommand_JSONDefaultFieldsExcludesDependencies(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	// --json with no value should use the default fields (valid,findings,workflows)
	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json", workflowPath,
	)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))

	// Default should include valid, findings, workflows
	assert.Contains(t, raw, "valid")
	assert.Contains(t, raw, "findings")
	assert.Contains(t, raw, "workflows")
	// Default should NOT include dependencies (opt-in only)
	assert.NotContains(t, raw, "dependencies", "default --json should not include top-level dependencies to avoid duplication with workflows")
}

// TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps verifies that
// SeedFromLockfile pre-warms the resolution cache so known deps skip
// network calls, while new deps still resolve from the network.
// The workflow has two deps: checkout (in lockfile) and setup-go (not in
// lockfile). Only setup-go should hit the HTTP mock.
func TestCheck_SeedFromLockfile_SkipsHTTPForCachedDeps(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	// Only register an HTTP stub for setup-go (the NEW dep).
	// No stub for checkout — the seed must serve it from cache.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

	// Workflow references both, but lockfile only has checkout.
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
      - uses: actions/setup-go@v6
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	// Lockfile records ONLY checkout — setup-go is "new".
	lockYAML := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v6:sha1-" + checkoutSHA + "':\n" +
		"    branch: 'main'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6:sha1-" + checkoutSHA + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))
	t.Chdir(dir)

	// Run WITHOUT --rescan so SeedFromLockfile is active.
	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-fix", "--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	// setup-go is resolved but not yet pinned → "not-pinned" finding → errSilent.
	// That's expected: we're testing that checkout was served from cache, not
	// that the overall check passes.
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The finding should be about setup-go being unpinned, NOT about checkout.
	// If checkout required an HTTP call, reg.Verify would fail (no stub registered).
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "not-pinned", payload.Findings[0].Category)
	assert.Contains(t, payload.Findings[0].Dependency, "setup-go")
}

// TestCheck_NoFix_WritesNothing proves --no-fix is read-only: it reports
// findings and returns a non-zero exit, but never touches the workflow or the
// lockfile on disk. setup-go is unpinned (a blocking finding under --no-fix),
// so the run exits errSilent while leaving both files byte-for-byte unchanged.
func TestCheck_NoFix_WritesNothing(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	checkoutSHA := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"

	// setup-go is the unpinned (new) dep — it resolves from the mock. checkout
	// is seeded from the lockfile and must not hit the network.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)

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
      - uses: actions/setup-go@v6
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))

	lockYAML := "version: 'v0.0.1'\ndependencies:\n" +
		"  'actions/checkout@v6:sha1-" + checkoutSHA + "':\n" +
		"    branch: 'main'\n    commit: 'sha1-" + checkoutSHA + "'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  '.github/workflows/workflow.yml':\n" +
		"    - 'actions/checkout@v6:sha1-" + checkoutSHA + "'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)

	wfBefore, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockBefore, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	// Terminal mode (no --json), read-only. setup-go is unpinned → !valid →
	// errSilent.
	_, _, runErr := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-fix", ".github/workflows/workflow.yml",
	)
	require.ErrorIs(t, runErr, errSilent)

	wfAfter, err := os.ReadFile(wfPath)
	require.NoError(t, err)
	lockAfter, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	assert.Equal(t, string(wfBefore), string(wfAfter), "--no-fix must not modify the workflow")
	assert.Equal(t, string(lockBefore), string(lockAfter), "--no-fix must not modify the lockfile")
}

// TestCheck_DefaultJSON_AutofixWrites proves --json is a renderer, not a
// read-only switch: a bare --json run (no --no-fix) still autofixes. setup-go
// is unpinned but fixable, so the pipeline pins it, writes the lockfile, and
// exits 0 — while stdout carries the JSON render. The single dep uses a
// full-semver ref (v6.0.0) so tag-narrowing skips the network; the remaining
// stubs cover the reverse-lookup (default branch → containing branch → tag)
// and the lockfile write (numeric owner/repo IDs).
func TestCheck_DefaultJSON_AutofixWrites(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	setupGoSHA := "4a3601121dd01d1626a1e23e37211e3254c1c06c"
	mainSHA := "1111111111111111111111111111111111111111"

	// Resolve setup-go@v6.0.0 → setupGoSHA.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "setup-go"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/setup-go", setupGoSHA, nodeActionYAML),
			},
		}),
	)
	// Reverse lookup: default branch HEAD (main), then compare confirms the
	// SHA is contained, then tags maps the SHA back to v6.0.0.
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
	// GetDefaultBranch (reverse lookup) and RepoIDs (lockfile write) now share
	// a single repos/{owner}/{repo} fetch, so one stub serves both. It carries
	// the fields either caller reads.
	repoMeta := httpmock.JSONResponse(map[string]any{
		"default_branch": "main",
		"id":             2,
		"owner":          map[string]any{"id": 1},
	})
	reg.Register(httpmock.REST("GET", `repos/actions/setup-go$`), repoMeta)

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfBody := strings.TrimSpace(`
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v6.0.0
`) + "\n"
	wfPath := filepath.Join(dir, ".github", "workflows", "workflow.yml")
	require.NoError(t, os.WriteFile(wfPath, []byte(wfBody), 0o600))
	t.Chdir(dir)

	// Bare --json: renderer only, autofix still runs.
	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--json=valid,findings",
		".github/workflows/workflow.yml",
	)
	require.NoError(t, err)

	// stdout is the JSON render — proves --json still selects the renderer.
	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// The lockfile gained the setup-go pin — proves --json no longer implies
	// read-only.
	pins := readTempLockfilePins(t)
	assert.Contains(t, pins, "actions/setup-go", "autofix should have pinned setup-go")
}

// TestCheck_Rescan_DetectsRefMovementDespiteLockfile is a regression test
// ensuring --rescan does NOT seed the resolution cache. If seeding occurred,
// the resolver would return the stale lockfile SHA and the ref-moved finding
// would be suppressed — exactly the bug we fixed.
func TestCheck_Rescan_DetectsRefMovementDespiteLockfile(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	staleSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	liveSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// HTTP returns the LIVE SHA (different from what the lockfile recorded).
	reg.Register(
		httpmock.GraphQLForRepo("example", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("example/action", liveSHA, nodeActionYAML),
			},
		}),
	)

	// Compare API: staleSHA is a legitimate ancestor of liveSHA (tag moved forward).
	reg.Register(
		httpmock.REST("GET", "repos/example/action/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": staleSHA,
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: example/action@v1
`,
		"example/action@v1:sha1-"+staleSHA,
	)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--rescan", "--no-fix", "--json=valid,findings", workflowPath,
	)
	// ref-moved is a warning (valid=true), not an error.
	require.NoError(t, err, "ref-moved is a warning, should not error")

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref-moved is a warning")

	hasRefMoved := false
	for _, f := range payload.Findings {
		if f.Category == "ref-moved" {
			hasRefMoved = true
		}
	}
	assert.True(t, hasRefMoved,
		"--rescan must detect ref movement (stale lockfile SHA vs live SHA); "+
			"if this fails, SeedFromLockfile is poisoning the resolution cache during rescan: %+v",
		payload.Findings)
}

func TestCheckCommand_JSONDeduplicatesDependencies(t *testing.T) {
	// When two workflow files share the same dep, top-level dependencies
	// should deduplicate them.
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// No HTTP stub: both workflows are fully recorded in the lockfile, so
	// the fast path skips every network round-trip. The dedup logic under
	// test operates purely on the inventory built from disk.

	wf1 := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`,
		"actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd",
	)

	wf2Path := filepath.Join(filepath.Dir(wf1), "workflow2.yml")
	require.NoError(t, os.WriteFile(wf2Path, []byte(strings.TrimSpace(`
name: deploy
on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`)+"\n"), 0o600))

	// Add wf2's deps to the lockfile (writeTempWorkflow only seeded wf1).
	writeTempLockfile(t, ".", "workflow.yml",
		[]string{"actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd"})
	// Replace with a multi-workflow lockfile.
	lockYAML := "version: v0.0.1\n" +
		"dependencies:\n" +
		"  actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd:\n" +
		"    branch: main\n    commit: sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  .github/workflows/workflow.yml:\n" +
		"    - actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n" +
		"  .github/workflows/workflow2.yml:\n" +
		"    - actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n"
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--no-fix", "--json=dependencies", wf1, wf2Path,
	)
	require.NoError(t, err)

	var payload struct {
		Dependencies []format.Dependency `json:"dependencies"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))

	// Should be deduplicated — 1 dep, not 2
	assert.Len(t, payload.Dependencies, 1, "top-level dependencies should be deduplicated across workflows")
	assert.Equal(t, "actions/checkout", payload.Dependencies[0].NWO)
}

func TestRootCommand_JSONExpandsDirectoryArguments(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	dir := t.TempDir()
	first := filepath.Join(dir, "first.yml")
	second := filepath.Join(dir, "second.yaml")
	ignored := filepath.Join(dir, "notes.txt")
	workflow := []byte(strings.TrimSpace(`
name: run only
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ok
`) + "\n")
	require.NoError(t, os.WriteFile(first, workflow, 0o600))
	require.NoError(t, os.WriteFile(second, workflow, 0o600))
	require.NoError(t, os.WriteFile(ignored, []byte("not a workflow\n"), 0o600))

	stdout, _, err := runCommandWithHTTP(t, reg, "--no-fix", "--json=valid,workflows", dir)
	require.NoError(t, err)

	var payload struct {
		Valid     bool `json:"valid"`
		Workflows []struct {
			Path  string `json:"path"`
			Valid bool   `json:"valid"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	require.Len(t, payload.Workflows, 2)
	assert.Equal(t, first, payload.Workflows[0].Path)
	assert.Equal(t, second, payload.Workflows[1].Path)
}

func TestCheckCommand_JSONLoadErrorIsInvalid(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	missingPath := filepath.Join(t.TempDir(), "missing.yml")
	stdout, _, err := runCommandWithHTTP(t, reg, "check", "--no-fix", "--json=valid,findings", missingPath)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool             `json:"valid"`
		Findings []format.Finding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "error", payload.Findings[0].Severity)
}
