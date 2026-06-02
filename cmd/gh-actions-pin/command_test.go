package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/resolver"
	"github.com/github/gh-actions-pin/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// branchListResponse builds a REST list-branches response body for httpmock.
// pairs are (name, sha) alternating: branchListResponse("main", "abc", "dev", "def")
// All entries are marked protected:true so tests model trusted-upstream branches.
func branchListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":      pairs[i],
			"commit":    map[string]any{"sha": pairs[i+1]},
			"protected": true,
		})
	}
	return out
}

// tagListResponse builds a REST list-tags response body for httpmock.
// pairs are (name, sha) alternating.
func tagListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":   pairs[i],
			"commit": map[string]any{"sha": pairs[i+1]},
		})
	}
	return out
}

// compareAncestorResponse builds a Compare API response indicating sha is an ancestor.
func compareAncestorResponse(mergeBaseSHA string) any {
	return map[string]any{
		"status":            "ahead",
		"merge_base_commit": map[string]any{"sha": mergeBaseSHA},
	}
}

func TestUpgradeCommand_WriteWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`refs\(refPrefix: "refs/tags/"`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"refs": map[string]any{
						"nodes": []map[string]string{
							{"name": "v4"},
							{"name": "v5"},
							{"name": "v6"},
						},
					},
				},
			},
		}),
	)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "34e114876b0b11c390a56381ad16ebd13914f8d5", nodeActionYAML),
				"a1": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
				"a2": testRepoResponse("actions/setup-go", "4a3601121dd01d1626a1e23e37211e3254c1c06c", nodeActionYAML),
			},
		}),
	)

	// NormalizeContaining: discover branch/tag for each resolved dep.
	// actions/checkout: two SHAs (v4 and v6), no exact HEAD match → compare path.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "co-head-sha")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/34e114`),
		httpmock.JSONResponse(compareAncestorResponse("34e114876b0b11c390a56381ad16ebd13914f8d5")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/de0fac`),
		httpmock.JSONResponse(compareAncestorResponse("de0fac2e4500dabe0009e67214ff5f5447ce83dd")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse(
			"v4", "34e114876b0b11c390a56381ad16ebd13914f8d5",
			"v6", "de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		)),
	)
	// actions/setup-go: exact HEAD match → no compare needed.
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/branches`),
		httpmock.JSONResponse(branchListResponse("main", "4a3601121dd01d1626a1e23e37211e3254c1c06c")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/setup-go/tags`),
		httpmock.JSONResponse(tagListResponse("v6", "4a3601121dd01d1626a1e23e37211e3254c1c06c")),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
`,
		"actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5",
		"actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd",
		"actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c",
	)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"upgrade", "--action", "actions/checkout", "--from", "v5", "--write", workflowPath,
	)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "actions/checkout: v5 -> v6")
	assert.Contains(t, stderr, "Upgraded 1 action(s)")

	content, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)

	got := string(content)
	assert.Contains(t, got, "uses: actions/checkout@v4")
	assert.Contains(t, got, "uses: actions/checkout@v6")
	assert.NotContains(t, got, "uses: actions/checkout@v5")
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")
}

func TestCheckCommand_JSONWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
				"a1": testRepoResponse("actions/setup-go", "4a3601121dd01d1626a1e23e37211e3254c1c06c", nodeActionYAML),
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

// reachableFunc returns a checkReachFn that reports all commits as reachable.
func reachableFunc() func(string, string, string, string) (resolver.ReachabilityStatus, string) {
	return func(owner, repo, sha, ref string) (resolver.ReachabilityStatus, string) {
		return resolver.Reachable, "ancestor of " + ref
	}
}

// unreachableFunc returns a checkReachFn that reports all commits as unreachable.
func unreachableFunc() func(string, string, string, string) (resolver.ReachabilityStatus, string) {
	return func(owner, repo, sha, ref string) (resolver.ReachabilityStatus, string) {
		return resolver.Unreachable, "commit is not an ancestor of " + ref
	}
}

// unknownReachFunc returns a checkReachFn that reports unknown (clone failure).
func unknownReachFunc() func(string, string, string, string) (resolver.ReachabilityStatus, string) {
	return func(owner, repo, sha, ref string) (resolver.ReachabilityStatus, string) {
		return resolver.ReachabilityUnknown, "clone failed"
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
		sb.WriteString("  '" + pin + "':\n    owner_id: 1\n    repo_id: 1\n")
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

func runCommandWithHTTPAndReach(t *testing.T, rt http.RoundTripper, reachFn func(string, string, string, string) (resolver.ReachabilityStatus, string), args ...string) (string, string, error) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	f := &pinFactory{
		Out:    stdoutW,
		ErrOut: stderrW,
		UI:     ui.NewPlain(stderrW),
		NewResolver: func(hostname string) (*resolver.Resolver, error) {
			r, err := resolver.NewWithTransport(hostname, rt)
			if err != nil {
				return nil, err
			}
			if reachFn != nil {
				r.SetCheckReachabilityFunc(reachFn)
			}
			return r, nil
		},
		IsTerminal: func() bool { return false },
	}

	cmd := NewRootCmd(f)
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are invalid")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ref_moved"], "should detect SHA changed: %+v", payload.Findings)
	assert.True(t, categories["imposter_commit"], "should detect unreachable commit: %+v", payload.Findings)
}

// TestCheck_UnreachableOnly verifies that when a pinned SHA matches live
// resolution but is not reachable from the ref, an IMPOSTER_COMMIT error is reported.
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero when findings are invalid")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	hasUnreachable := false
	for _, f := range payload.Findings {
		if f.Category == "imposter_commit" {
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "unknown reachability should not fail the check")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "valid should be true when reachability is unknown")

	// Reachability unknown produces a CategoryValid finding with SeverityWarning.
	hasWarning := false
	for _, f := range payload.Findings {
		if f.Severity == "warning" && strings.Contains(f.Detail, "clone failed") {
			hasWarning = true
		}
	}
	assert.True(t, hasWarning, "should have a reachability warning: %+v", payload.Findings)
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Findings)
}

// ==========================================================================
// Lockfile Forgery Detection Tests
//
// These tests verify that the ancestry check promotes REF_MOVED to
// LOCKFILE_FORGERY when the pinned SHA is not an ancestor of the live SHA.
// This detects cases where someone manually injected a SHA into the lockfile
// that was never part of the ref's legitimate history.
// ==========================================================================

// TestCheck_LockfileForgery_NotAncestor verifies that when the Compare API
// shows the pinned SHA is NOT an ancestor of the live SHA, the finding is
// promoted from REF_MOVED to LOCKFILE_FORGERY.
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.ErrorIs(t, err, errSilent, "JSON mode should exit non-zero for forgery findings")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["lockfile_forgery"], "should detect lockfile forgery: %+v", payload.Findings)
	assert.False(t, categories["ref_moved"], "should NOT have ref_moved (promoted to forgery): %+v", payload.Findings)
}

// TestCheck_LockfileForgery_LegitAncestor verifies that when the Compare API
// confirms the pinned SHA IS an ancestor of the live SHA, the finding stays
// as REF_MOVED (legitimate tag movement, not forgery).
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref_moved is a warning, should not error")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref_moved is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ref_moved"], "should keep as ref_moved for legit ancestor: %+v", payload.Findings)
	assert.False(t, categories["lockfile_forgery"], "should NOT have lockfile_forgery: %+v", payload.Findings)
}

// TestCheck_LockfileForgery_RateLimited verifies that when the ancestry check
// is rate-limited, the finding stays as REF_MOVED (fail open).
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
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "ref_moved is a warning, should not error")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "ref_moved is a warning, workflow is still valid")

	categories := map[string]bool{}
	for _, f := range payload.Findings {
		categories[f.Category] = true
	}
	assert.True(t, categories["ref_moved"], "should keep as ref_moved when rate limited: %+v", payload.Findings)
	assert.False(t, categories["lockfile_forgery"], "should NOT have lockfile_forgery when rate limited: %+v", payload.Findings)

	// Verify the detail mentions the inconclusive ancestry check.
	for _, f := range payload.Findings {
		if f.Category == "ref_moved" {
			assert.Contains(t, f.Detail, "ancestry check inconclusive")
		}
	}
}

func TestCheckCommand_JSONDependenciesWithRequiredBy(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	compositeYAML := "name: Setup Go\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/save@v4\n"

	// First query: direct refs (checkout + setup-go).
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
				"a1": testRepoResponse("actions/setup-go", "d35c59abb061a4a6fb18e82ac0862c26744d6ab5", compositeYAML),
			},
		}),
	)
	// Second query: transitive dep discovered from composite (cache/save).
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
		"check", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Path         string            `json:"path"`
			Valid        bool              `json:"valid"`
			Dependencies []checkDependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	wf := payload.Workflows[0]
	assert.True(t, wf.Valid)
	require.Len(t, wf.Dependencies, 3)

	// Find the transitive dep
	var transitiveDep *checkDependency
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
		"check", "--json=workflows", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Workflows []struct {
			Dependencies []checkDependency `json:"dependencies"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	require.Len(t, payload.Workflows, 1)

	var transitiveDep *checkDependency
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
		"check", "--json", workflowPath,
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

func TestCheckCommand_JSONDeduplicatesDependencies(t *testing.T) {
	// When two workflow files share the same dep, top-level dependencies
	// should deduplicate them.
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Single mock — the resolver caches, so both workflows resolve via one query.
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
			},
		}),
	)

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
		"    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n" +
		"  .github/workflows/workflow.yml:\n" +
		"    - actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n" +
		"  .github/workflows/workflow2.yml:\n" +
		"    - actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd\n"
	require.NoError(t, os.WriteFile(filepath.Join(".github", "workflows", "actions.lock"), []byte(lockYAML), 0o600))

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, reachableFunc(),
		"check", "--json=dependencies", wf1, wf2Path,
	)
	require.NoError(t, err)

	var payload struct {
		Dependencies []checkDependency `json:"dependencies"`
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

	stdout, _, err := runCommandWithHTTP(t, reg, "--json=valid,workflows", dir)
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
	stdout, _, err := runCommandWithHTTP(t, reg, "check", "--json=valid,findings", missingPath)
	require.ErrorIs(t, err, errSilent)

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.Len(t, payload.Findings, 1)
	assert.Equal(t, "error", payload.Findings[0].Severity)
}
