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
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", "34e114876b0b11c390a56381ad16ebd13914f8d5", nodeActionYAML),
				"a1": testRepoResponse("actions/checkout", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", nodeActionYAML),
				"a2": testRepoResponse("actions/setup-go", "4a3601121dd01d1626a1e23e37211e3254c1c06c", nodeActionYAML),
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
      - uses: actions/checkout@v4
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v4:sha1-34e114876b0b11c390a56381ad16ebd13914f8d5
  - github.com/actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd
  - github.com/actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c
`)

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
	assert.Contains(t, got, "github.com/actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")
}

func TestCheckCommand_JSONWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd
  - github.com/actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c
`)

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

func writeTempWorkflow(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "workflow.yml")
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600))
	return path
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
		httpmock.GraphQL(`repository\(owner: "example", name: "action"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/example/action@v1:sha1-`+pinnedSHA+`
`)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, unreachableFunc(),
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

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
	assert.True(t, categories["tampered"], "should detect SHA changed: %+v", payload.Findings)
	assert.True(t, categories["unreachable"], "should detect unreachable commit: %+v", payload.Findings)
}

// TestCheck_UnreachableOnly verifies that when a pinned SHA matches live
// resolution but is not reachable from the ref, an UNREACHABLE error is reported.
func TestCheck_UnreachableOnly(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "cccccccccccccccccccccccccccccccccccccccc"

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "example", name: "action"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/example/action@v1:sha1-`+sha+`
`)

	stdout, _, err := runCommandWithHTTPAndReach(t, reg, unreachableFunc(),
		"check", "--json=valid,findings", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid    bool           `json:"valid"`
		Findings []checkFinding `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	hasUnreachable := false
	for _, f := range payload.Findings {
		if f.Category == "unreachable" {
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
		httpmock.GraphQL(`repository\(owner: "example", name: "action"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/example/action@v1:sha1-`+sha+`
`)

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
		httpmock.GraphQL(`repository\(owner: "example", name: "action"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/example/action@v1:sha1-`+sha+`
`)

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
