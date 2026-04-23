package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/resolver"
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
	assert.Contains(t, stderr, "Planning upgrade(s)...")
	assert.Contains(t, stderr, "actions/checkout: v5 -> v6")
	assert.Contains(t, stderr, "Upgraded and pinned 3 dependencies")

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
	compareReachable(reg, `/repos/actions/checkout/compare/`)
	compareReachable(reg, `/repos/actions/setup-go/compare/`)

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

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors,warnings", workflowPath,
	)
	require.NoError(t, err)
	assert.Contains(t, stderr, "Re-resolving 2 action reference(s)...")

	var payload struct {
		Valid    bool              `json:"valid"`
		Errors   []validationError `json:"errors"`
		Warnings []string          `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Errors)
	assert.Empty(t, payload.Warnings)
}

func TestPinCommand_PreviewWithHTTPMocks(t *testing.T) {
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
`)

	stdout, stderr, err := runCommandWithHTTP(t, reg, workflowPath)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Resolving 2 action reference(s)...")
	assert.Contains(t, stderr, "Preview summary for")
	assert.Contains(t, stderr, "direct: 2 added")
	assert.Contains(t, stderr, "Apply with:  gh actions-pin --write")
}

func TestPinCommand_WriteRejectsDirectRefChangesByDefault(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd
`)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--write", workflowPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errSilent))
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "refusing to bless them with --write")
	assert.Contains(t, stderr, "hint: use `gh actions-pin upgrade --action actions/checkout --from v5 --version v6 --write`")

	content, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "uses: actions/checkout@v6")
	assert.Contains(t, string(content), "github.com/actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd")
}

func TestPinCommand_WriteAllowsAcknowledgedRefChanges(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
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

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v5:sha1-93cb6efe18208431cddfb8368fd83d5badbf9bfd
`)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--write", "--allow-ref-changes", workflowPath)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Pinned 1 dependencies")

	content, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "github.com/actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")
}

func TestUpdateCommand_TargetedRefreshWithHTTPMocks(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
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
      - uses: actions/setup-go@v6

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v6:sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  - github.com/actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c
`)

	stdout, stderr, err := runCommandWithHTTP(t, reg,
		"update", "--action", "github.com/actions/checkout", "--write", workflowPath,
	)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Pinned 2 dependencies")

	content, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	got := string(content)
	assert.Contains(t, got, "github.com/actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")
	assert.Contains(t, got, "github.com/actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c")
}

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

// compareReachable registers a REST compare stub that returns "identical" for
// any compare request matching the given path pattern, simulating a reachable commit.
func compareReachable(reg *httpmock.Registry, pathPattern string) {
	reg.Register(
		httpmock.REST("GET", pathPattern),
		httpmock.JSONResponse(map[string]any{"status": "identical"}),
	)
}

// compareUnreachable registers a REST compare stub that returns "diverged",
// simulating a fork-network injected commit.
func compareUnreachable(reg *httpmock.Registry, pathPattern string) {
	reg.Register(
		httpmock.REST("GET", pathPattern),
		httpmock.JSONResponse(map[string]any{"status": "diverged"}),
	)
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
	t.Helper()

	oldResolver := newResolver
	newResolver = func(hostname string) (*resolver.Resolver, error) {
		return resolver.NewWithTransport(hostname, rt)
	}
	defer func() {
		newResolver = oldResolver
	}()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	stderrR, stderrW, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = stdoutW
	os.Stderr = stderrW

	cmd := newRootCmd()
	cmd.SetArgs(args)
	runErr := cmd.Execute()

	_ = stdoutW.Close()
	_ = stderrW.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

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
func TestCheck_TjActionsChangedFiles_TagMutationAttack(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// The legitimate pinned SHA from before the attack
	legitimateSHA := "4edd678ac3f81e2dc578756871e4d00c19191c4e"
	// The attacker's SHA that the tag was force-pushed to
	maliciousSHA := "0e58ed8671d6b60d0890c21b07f8835ace038e67"

	// Live resolution returns the MALICIOUS SHA (tag was moved)
	reg.Register(
		httpmock.GraphQL(`repository\(owner: "tj-actions", name: "changed-files"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("tj-actions/changed-files", maliciousSHA, nodeActionYAML),
			},
		}),
	)
	// The malicious SHA is diverged from the legitimate branch
	compareUnreachable(reg, `/repos/tj-actions/changed-files/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: tj-actions/changed-files@v44

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/tj-actions/changed-files@v44:sha1-`+legitimateSHA+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid  bool              `json:"valid"`
		Errors []validationError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	// Should detect TAMPERED (SHA changed) AND UNREACHABLE (fork-network)
	errorTypes := map[string]bool{}
	for _, e := range payload.Errors {
		errorTypes[e.Type] = true
	}
	assert.True(t, errorTypes["TAMPERED"], "should detect SHA tamper: %+v", payload.Errors)
	assert.True(t, errorTypes["UNREACHABLE"], "should detect unreachable commit: %+v", payload.Errors)
}

// TestCheck_ReviewdogActionSetup_ForkNetworkInjection models the reviewdog
// attack where a malicious commit from a fork was referenced via tag
// manipulation. The commit exists in GitHub's shared object store but is
// NOT reachable from the canonical repository's refs.
func TestCheck_ReviewdogActionSetup_ForkNetworkInjection(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Attacker's commit from a fork - exists in the network but not the canonical repo
	forkNetworkSHA := "b0c14eb73e15d54af9e97eb7fe20e74fa238fd07"

	// Live resolution returns the fork-network SHA (tag was moved to it)
	reg.Register(
		httpmock.GraphQL(`repository\(owner: "reviewdog", name: "action-setup"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("reviewdog/action-setup", forkNetworkSHA, nodeActionYAML),
			},
		}),
	)
	// Compare returns diverged: SHA exists in fork network but not on ref lineage
	compareUnreachable(reg, `/repos/reviewdog/action-setup/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: lint
on: pull_request
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: reviewdog/action-setup@v1

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/reviewdog/action-setup@v1:sha1-`+forkNetworkSHA+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid  bool              `json:"valid"`
		Errors []validationError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	// Even though the SHA matches live resolution (tag still points to malicious commit),
	// the reachability check catches the fork-network injection.
	hasUnreachable := false
	for _, e := range payload.Errors {
		if e.Type == "UNREACHABLE" {
			hasUnreachable = true
			assert.Contains(t, e.Details, "not reachable")
		}
	}
	assert.True(t, hasUnreachable, "should detect fork-network injected commit: %+v", payload.Errors)
}

// TestCheck_XygeniAction_TagPoisoningWithBackdoor models the xygeni-action
// compromise where a tag was poisoned to inject a C2 reverse shell backdoor.
// The malicious commit is from outside the canonical repo's history.
func TestCheck_XygeniAction_TagPoisoningWithBackdoor(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	maliciousSHA := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "xygeni", name: "xygeni-action"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("xygeni/xygeni-action", maliciousSHA, nodeActionYAML),
			},
		}),
	)
	// The poisoned SHA doesn't exist in the canonical repo at all (404)
	reg.Register(
		httpmock.REST("GET", `/repos/xygeni/xygeni-action/compare/`),
		httpmock.StatusResponse(404),
	)

	workflowPath := writeTempWorkflow(t, `
name: security-scan
on: push
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: xygeni/xygeni-action@v3

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/xygeni/xygeni-action@v3:sha1-`+maliciousSHA+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid  bool              `json:"valid"`
		Errors []validationError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	hasUnreachable := false
	for _, e := range payload.Errors {
		if e.Type == "UNREACHABLE" {
			hasUnreachable = true
			assert.Contains(t, e.Details, "not found in repository")
		}
	}
	assert.True(t, hasUnreachable, "should detect SHA not in canonical repo: %+v", payload.Errors)
}

// TestCheck_TrivyAction_ScannerToStealer models the aquasecurity/trivy-action
// compromise where the tag was manipulated to redirect to a malicious version
// that exfiltrated secrets instead of scanning.
func TestCheck_TrivyAction_ScannerToStealer(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	legitimateSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	maliciousSHA := "cafebabecafebabecafebabecafebabecafebabe"

	// Live resolution returns the malicious SHA (tag was moved)
	reg.Register(
		httpmock.GraphQL(`repository\(owner: "aquasecurity", name: "trivy-action"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("aquasecurity/trivy-action", maliciousSHA, nodeActionYAML),
			},
		}),
	)
	// The malicious SHA diverges from the legitimate lineage
	compareUnreachable(reg, `/repos/aquasecurity/trivy-action/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: security
on: push
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: aquasecurity/trivy-action@master

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/aquasecurity/trivy-action@master:sha1-`+legitimateSHA+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid  bool              `json:"valid"`
		Errors []validationError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	errorTypes := map[string]bool{}
	for _, e := range payload.Errors {
		errorTypes[e.Type] = true
	}
	assert.True(t, errorTypes["TAMPERED"], "should detect SHA changed: %+v", payload.Errors)
	assert.True(t, errorTypes["UNREACHABLE"], "should detect fork-network SHA: %+v", payload.Errors)
}

// TestCheck_CheckmarxKICS_TagForceViaStoredCreds models the March 2026 Checkmarx
// KICS compromise where credentials stolen during the Trivy breach were used to
// force-push malicious code to KICS GitHub Action tags (TeamPCP lateral movement).
func TestCheck_CheckmarxKICS_TagForceViaStoredCreds(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	legitimateSHA := "1111111111111111111111111111111111111111"
	maliciousSHA := "2222222222222222222222222222222222222222"

	// Live resolution returns the malicious SHA (tag force-pushed with stolen creds)
	reg.Register(
		httpmock.GraphQL(`repository\(owner: "Checkmarx", name: "kics-github-action"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("Checkmarx/kics-github-action", maliciousSHA, nodeActionYAML),
			},
		}),
	)
	// The malicious SHA diverges from the legitimate lineage
	compareUnreachable(reg, `/repos/Checkmarx/kics-github-action/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: sast
on: push
jobs:
  kics:
    runs-on: ubuntu-latest
    steps:
      - uses: Checkmarx/kics-github-action@v2

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/Checkmarx/kics-github-action@v2:sha1-`+legitimateSHA+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors", workflowPath,
	)
	require.NoError(t, err, "JSON mode communicates errors in payload")

	var payload struct {
		Valid  bool              `json:"valid"`
		Errors []validationError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)

	errorTypes := map[string]bool{}
	for _, e := range payload.Errors {
		errorTypes[e.Type] = true
	}
	assert.True(t, errorTypes["TAMPERED"], "should detect SHA changed: %+v", payload.Errors)
	assert.True(t, errorTypes["UNREACHABLE"], "should detect fork-network SHA: %+v", payload.Errors)
}

// TestCheck_ReachabilityUnknown_DoesNotFailValidation verifies that when the
// compare endpoint returns an error (rate limit, timeout, etc.), the check
// command issues a warning but does NOT mark the validation as failed.
func TestCheck_ReachabilityUnknown_DoesNotFailValidation(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", sha, nodeActionYAML),
			},
		}),
	)
	// Simulate a 500 error from the compare endpoint
	reg.Register(
		httpmock.REST("GET", `/repos/actions/checkout/compare/`),
		httpmock.StatusResponse(500),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v6:sha1-`+sha+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors,warnings", workflowPath,
	)
	require.NoError(t, err, "unknown reachability should not fail the check")

	var payload struct {
		Valid    bool              `json:"valid"`
		Errors   []validationError `json:"errors"`
		Warnings []string          `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid, "valid should be true when reachability is unknown")
	assert.Empty(t, payload.Errors)
	assert.NotEmpty(t, payload.Warnings, "should have a reachability warning")
	assert.Contains(t, payload.Warnings[0], "reachability check inconclusive")
}

// TestCheck_Reachable_CleanValidation verifies the happy path: pinned SHA
// is reachable, live resolution matches, everything is valid.
func TestCheck_Reachable_CleanValidation(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", sha, nodeActionYAML),
			},
		}),
	)
	compareReachable(reg, `/repos/actions/checkout/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

# Automatically generated and managed by: gh actions-pin --write <workflow-path>
dependencies:
  - github.com/actions/checkout@v6:sha1-`+sha+`
`)

	stdout, _, err := runCommandWithHTTP(t, reg,
		"check", "--json", "valid,errors,warnings", workflowPath,
	)
	require.NoError(t, err)

	var payload struct {
		Valid    bool              `json:"valid"`
		Errors   []validationError `json:"errors"`
		Warnings []string          `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	assert.Empty(t, payload.Errors)
	assert.Empty(t, payload.Warnings)
}

// TestPin_UnreachableSHA_WarnsButDoesNotBlock verifies that when a freshly
// resolved SHA fails the reachability check during pinning, the CLI warns
// but does not block the pin operation (defense-in-depth, not a hard gate).
func TestPin_UnreachableSHA_WarnsButDoesNotBlock(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "de0fac2e4500dabe0009e67214ff5f5447ce83dd"

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("actions/checkout", sha, nodeActionYAML),
			},
		}),
	)
	compareUnreachable(reg, `/repos/actions/checkout/compare/`)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`)

	_, stderr, err := runCommandWithHTTP(t, reg, "--diff", workflowPath)
	require.NoError(t, err, "pin should succeed even with unreachable warning")
	assert.Contains(t, stderr, "NOT reachable")
	assert.Contains(t, stderr, "fork-network injection")
}
