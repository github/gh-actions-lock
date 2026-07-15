package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/github/gh-actions-lock/internal/workflowfile"
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
	require.ErrorIs(t, err, errSilent)
	assert.Zero(t, transport.calls.Load(), "invalid syntax should be rejected before dependency resolution")

	gotWorkflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)
	assert.NotContains(t, string(gotWorkflow), workflowfile.SentinelComment)

	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	if lockContent, readErr := os.ReadFile(lockPath); readErr == nil {
		assert.NotContains(t, string(lockContent), "actions/checkout")
	} else {
		assert.ErrorIs(t, readErr, os.ErrNotExist)
	}
}
