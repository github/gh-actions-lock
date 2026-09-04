package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type requestCountingTransport struct {
	calls atomic.Int64
}

func (t *requestCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected HTTP request")
}

func TestInvalidSelfRepositoryRefDoesNotPinOtherRefs(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/actions/bad@v1
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)
	// Migration runs by default and validates self repository refs first, so an
	// invalid `$/…` ref aborts the run before any dependency resolution.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid self repository reference")
	assert.Zero(t, transport.calls.Load(), "invalid syntax should be rejected before dependency resolution")

	gotWorkflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)

	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	if lockContent, readErr := os.ReadFile(lockPath); readErr == nil {
		assert.NotContains(t, string(lockContent), "actions/checkout")
	} else {
		assert.ErrorIs(t, readErr, os.ErrNotExist)
	}
}

func TestInvalidSelfRepositoryRefPreventsMigrationAndNetwork(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	localActionPath := filepath.Join(dir, "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(localActionPath), 0o755))
	require.NoError(t, os.WriteFile(localActionPath, []byte("runs:\n  using: composite\n"), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./actions/local
      - uses: "$/actions/bad@${{ matrix.ref }}"
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)

	require.Error(t, err)
	assert.Zero(t, transport.calls.Load(), "invalid syntax should stop migration before dependency resolution")
	gotWorkflow, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflow, gotWorkflow)
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	_, readErr = os.Stat(lockPath)
	assert.ErrorIs(t, readErr, os.ErrNotExist)
}

func TestExistingSHARefRewritesSelfRepositoryAction(t *testing.T) {
	const sha = "bcd2ba49218906704ab6c1aa796996da409d3eb1"
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	actionPath := filepath.Join(dir, ".github", "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
	require.NoError(t, os.WriteFile(actionPath, []byte(`name: Local
runs:
  using: composite
  steps:
    - uses: actions/create-github-app-token@`+sha+`
`), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/.github/actions/local
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workflowPath), "other.yml"), []byte(`name: Other
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/create-github-app-token@v3.2.0
`), 0o600))
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	originalLock := []byte(`version: 'v0.0.2'
workflows:
    '.github/workflows/ci.yml':
        - 'actions/create-github-app-token@` + sha + `'
    '.github/workflows/other.yml':
        - 'actions/create-github-app-token@v3.2.0'
dependencies:
    'actions/create-github-app-token@` + sha + `':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/checkout@v4'
    'actions/create-github-app-token@v3.2.0':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/setup-go@v5'
    'actions/checkout@v4':
        ref: 'v4'
        commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
        owner_id: 44036562
        repo_id: 197814629
    'actions/setup-go@v5':
        ref: 'v5'
        commit: 'sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
        owner_id: 44036562
        repo_id: 485264523
`)
	require.NoError(t, os.WriteFile(lockPath, originalLock, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, "--no-narrow", filepath.Join(".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	lockAfterNoNarrow, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	_, _, err = runCommandWithHTTP(t, transport, filepath.Join(".github", "workflows", "ci.yml"))
	require.ErrorContains(t, err, "partial workflow scan")
	action, readErr := os.ReadFile(actionPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(action), "uses: actions/create-github-app-token@"+sha)
	lock, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.Equal(t, lockAfterNoNarrow, lock)

	_, _, err = runCommandWithHTTP(t, transport)
	require.NoError(t, err)
	assert.Zero(t, transport.calls.Load())

	action, err = os.ReadFile(actionPath)
	require.NoError(t, err)
	assert.Contains(t, string(action), "uses: actions/create-github-app-token@v3.2.0")

	lock, err = os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Contains(t, string(lock), "'actions/create-github-app-token@v3.2.0'")
	assert.Contains(t, string(lock), "'actions/checkout@v4'")
	assert.Contains(t, string(lock), "'actions/setup-go@v5'")
	assert.Contains(t, string(lock), "uses:\n            - 'actions/checkout@v4'")
	assert.Contains(t, string(lock), "- 'actions/setup-go@v5'")
	assert.NotContains(t, string(lock), "'actions/create-github-app-token@"+sha+"'")
}
