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

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--diff", workflowPath)
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
		"update", "--action", "github.com/actions/checkout", workflowPath,
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

func TestTidyCommand_RemovesStaleEntries(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

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
  - github.com/actions/checkout@v6:sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd
  - github.com/actions/setup-go@v6:sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c
`)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "tidy", workflowPath)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Tidied 1 dependencies")
	assert.Contains(t, stderr, "removed 1 stale")

	content, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	got := string(content)
	assert.Contains(t, got, "github.com/actions/checkout@v6")
	assert.NotContains(t, got, "github.com/actions/setup-go@v6")
}

const nodeActionYAML = "name: Test Action\nruns:\n  using: node20\n"

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
