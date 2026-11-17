package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyRejectsSelfActionSHAMissingFromLockfile(t *testing.T) {
	path := writeTempWorkflow(t, `
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/setup
`, "actions/checkout@v4=sha1-"+parityTestSHA)
	require.NoError(t, os.Mkdir(".git", 0o755))
	require.NoError(t, os.Mkdir("setup", 0o755))
	actionPath := filepath.Join("setup", "action.yml")
	action := []byte("name: Setup\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@" + parityTestSHA + "\n")
	require.NoError(t, os.WriteFile(actionPath, action, 0o600))
	lockPath := filepath.Join(".github", "workflows", "actions.lock")
	before, err := os.ReadFile(lockPath)
	require.NoError(t, err)

	transport := &requestCountingTransport{parity: true}
	result, err := runParity(t, transport, "--verify", path)
	require.ErrorIs(t, err, errSilent)
	assert.False(t, result.Valid)
	var missing bool
	for _, finding := range result.Findings {
		if finding.Category == "not-pinned" {
			missing = true
			assert.Contains(t, finding.Detail, "actions/checkout@"+parityTestSHA)
		}
	}
	assert.True(t, missing)
	assert.Zero(t, transport.calls.Load())
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	got, err := os.ReadFile(actionPath)
	require.NoError(t, err)
	assert.Equal(t, action, got)
}
