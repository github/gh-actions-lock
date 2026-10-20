package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/cmd/gh-actions-lock/format"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalActionsAreAllowedWithoutFollowingOrRewriting(t *testing.T) {
	const workflow = `name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/local
`
	for _, fixture := range []struct {
		name, content string
	}{
		{name: "missing action"},
		{name: "malformed action", content: "runs: ["},
		{name: "unresolvable nested dependency", content: "runs:\n  using: composite\n  steps:\n    - uses: nonexistent/unreachable@missing\n"},
		{name: "nested workspace and invalid self refs", content: "runs:\n  using: composite\n  steps:\n    - uses: ./cycle\n    - uses: $/bad@v1\n"},
	} {
		for _, mode := range []struct {
			name string
			args []string
		}{
			{name: "lock"},
			{name: "sticky compatibility flag", args: []string{"--no-migrate-local-actions"}},
			{name: "relock", args: []string{"--relock"}},
			{name: "accept moved", args: []string{"--accept-moved"}},
			{name: "read only", args: []string{"--no-fix"}},
			{name: "verify", args: []string{"--verify"}},
			{name: "verify local", args: []string{"--verify-local"}},
		} {
			t.Run(fixture.name+"/"+mode.name, func(t *testing.T) {
				wf := writeTempWorkflow(t, workflow)
				writeTempLockfile(t, ".", filepath.Base(wf), nil)
				require.NoError(t, os.Mkdir(".git", 0o755))
				if fixture.content != "" {
					require.NoError(t, os.MkdirAll(".github/actions/local", 0o755))
					require.NoError(t, os.WriteFile(".github/actions/local/action.yml", []byte(fixture.content), 0o600))
				}
				before := snapshotWorkflows(t)
				transport := &requestCountingTransport{}
				args := append([]string{"--json=valid,findings"}, mode.args...)
				stdout, _, err := runCommandWithHTTP(t, transport, append(args, wf)...)
				require.NoError(t, err)
				assert.Zero(t, transport.calls.Load(), "nothing behind ./ may be resolved")
				var payload struct {
					Valid    bool             `json:"valid"`
					Findings []format.Finding `json:"findings"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
				assert.True(t, payload.Valid)
				require.Len(t, payload.Findings, 1)
				assert.Equal(t, "local-action", payload.Findings[0].Category)
				assert.Equal(t, "info", payload.Findings[0].Severity)
				assert.Contains(t, payload.Findings[0].Detail, "never followed or locked")
				assert.Contains(t, payload.Findings[0].Detail, "uses: dependencies are not covered")
				_, stderr, err := runCommandWithHTTP(t, transport, append(mode.args, wf)...)
				require.NoError(t, err)
				assert.Contains(t, stderr, "never followed or locked")
				assert.Contains(t, stderr, "uses: dependencies are not covered")
				assert.NotContains(t, stderr, "complete lockfile coverage")
				assert.Zero(t, transport.calls.Load())
				after := snapshotWorkflows(t)
				if mode.name == "read only" || mode.name == "verify" || mode.name == "verify local" {
					assert.Equal(t, before, after, "read-only modes must not write any file")
				}
				delete(before, ".github/workflows/actions.lock")
				delete(after, ".github/workflows/actions.lock")
				assert.Equal(t, before, after, "workflow and action files must remain byte-identical")
			})
		}
	}
}

func TestLocalActionsCanBeOnboardedWithNoLocalManifest(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./absent\n")
	transport := &requestCountingTransport{}
	_, stderr, err := runCommandWithHTTP(t, transport, wf)
	require.NoError(t, err)
	assert.Contains(t, stderr, "never followed or locked")
	assert.Contains(t, readTempLockfilePins(t), "'"+wf+"': []")
	assert.Zero(t, transport.calls.Load())
}

func TestLocalActionDoesNotHideUnpinnedRemoteAction(t *testing.T) {
	wf := writeTempWorkflow(t, "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./absent\n      - uses: owner/action@v1\n")
	writeTempLockfile(t, ".", filepath.Base(wf), nil)
	stdout, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify-local", "--json=valid,findings", wf)
	require.ErrorIs(t, err, errSilent)
	assert.Contains(t, stdout, `"valid": false`)
	assert.Contains(t, stdout, `"category": "local-action"`)
	assert.Contains(t, stdout, `"category": "not-pinned"`)
	assert.Contains(t, stdout, "owner/action@v1")
}

func TestRemoteCompositeLocalActionsRemainVisibleAcrossOnlineModes(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "initial lock"},
		{name: "sticky"},
		{name: "verify", args: []string{"--verify"}},
		{name: "read only", args: []string{"--no-fix"}},
		{name: "relock", args: []string{"--relock"}},
		{name: "accept moved", args: []string{"--accept-moved"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			wf := writeTempWorkflow(t, cleanWorkflow)
			reg := &httpmock.Registry{}
			registerSetupGoManifest(reg, "runs:\n  using: composite\n  steps:\n    - uses: ./workspace-helper\n")
			if mode.name != "initial lock" {
				stdout, stderr, err := runCommandWithHTTP(t, reg, "--json=valid,findings,pins", wf)
				require.NoError(t, err, stdout, stderr)
				reg = &httpmock.Registry{}
				registerParity(reg)
				registerSetupGoManifest(reg, "runs:\n  using: composite\n  steps:\n    - uses: ./workspace-helper\n")
			}
			args := append([]string{"--json=valid,findings,pins"}, mode.args...)
			stdout, _, err := runCommandWithHTTP(t, reg, append(args, wf)...)
			require.NoError(t, err, stdout)
			var payload struct {
				Valid    bool             `json:"valid"`
				Findings []format.Finding `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
			if mode.name == "initial lock" {
				assert.False(t, payload.Valid, "JSON retains pre-fix findings")
				require.Len(t, payload.Findings, 2)
			} else {
				assert.True(t, payload.Valid, stdout)
				require.Len(t, payload.Findings, 1)
			}
			assert.Equal(t, "local-action", payload.Findings[0].Category)
			assert.Equal(t, "info", payload.Findings[0].Severity)
			assert.Contains(t, payload.Findings[0].Detail, "actions/setup-go@v5.0.0")
			assert.Contains(t, payload.Findings[0].Detail, "./workspace-helper")
			assert.Contains(t, payload.Findings[0].Detail, "uses: dependencies are not covered")
		})
	}
}
