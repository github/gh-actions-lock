package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeParallelStepsRepo(t *testing.T, workflow string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "workflow.yml"), []byte(workflow), 0o600))
	lockYAML := "version: '" + parserlock.Version + "'\ndependencies:\n" +
		"  'actions/checkout@v6':\n    ref: 'v6'\n    commit: 'sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd'\n    owner_id: 1\n    repo_id: 1\n" +
		"  'actions/setup-go@v6':\n    ref: 'v6'\n    commit: 'sha1-4a3601121dd01d1626a1e23e37211e3254c1c06c'\n    owner_id: 1\n    repo_id: 1\n" +
		"workflows:\n  '.github/workflows/workflow.yml':\n    - 'actions/checkout@v6'\n    - 'actions/setup-go@v6'\n"
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lockYAML), 0o600))
	t.Chdir(dir)
	return lockPath
}

func TestCheck_ParallelStepGroups(t *testing.T) {
	t.Run("full scan keeps a dependency used only in a parallel group", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		registerNodeManifests(reg)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - parallel:
          - uses: actions/setup-go@v6
          - run: echo hi
`)
		_, stderr, err := runCommandWithHTTP(t, reg, "--json=valid")
		require.NoError(t, err, stderr)
		got, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Contains(t, string(got), "'actions/setup-go@v6'")
	})

	t.Run("full scan drops a dependency removed from a parallel group", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - parallel:
          - uses: actions/checkout@v6
          - run: echo hi
`)
		_, _, err := runCommandWithHTTP(t, reg, "--json=valid")
		require.NoError(t, err)
		got, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Contains(t, string(got), "'actions/checkout@v6'")
		assert.NotContains(t, string(got), "setup-go")
	})

	t.Run("verify-local rejects a parallel action missing from the lockfile", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		lockPath := writeParallelStepsRepo(t, `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - parallel:
          - uses: actions/setup-go@v6
          - uses: actions/setup-node@v6
`)
		before, err := os.ReadFile(lockPath)
		require.NoError(t, err)

		stdout, _, _ := runCommandWithHTTP(t, reg, "--verify-local", "--json=valid,findings")
		var payload struct {
			Valid    bool             `json:"valid"`
			Findings []format.Finding `json:"findings"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &payload), stdout)
		assert.False(t, payload.Valid)
		var flagged bool
		for _, f := range payload.Findings {
			if f.Severity == "error" && f.Dependency == "actions/setup-node@v6" {
				flagged = true
			}
		}
		assert.True(t, flagged, "findings: %+v", payload.Findings)

		after, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after))
	})
}
