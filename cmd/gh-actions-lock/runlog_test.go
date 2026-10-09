package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteRunLog(t *testing.T) {
	t.Run("records a failed run as invalid with findings and pin outcomes", func(t *testing.T) {
		dir := t.TempDir()
		report := &checks.Report{Workflows: []checks.WorkflowReport{{
			Path: ".github/workflows/ci.yml",
			Findings: []checks.Finding{{
				Category: checks.LocalAction,
				Severity: checks.SeverityError,
				Detail:   "local path cannot be resolved",
			}},
		}}}
		record := &pin.Record{Entries: []pin.Entry{
			{NWO: "o/r", Ref: "v1", Resolution: pin.Unresolved, Reason: "not found", Workflows: []string{"a.yml"}},
			{NWO: "O/R", Ref: "v1", Resolution: pin.Unresolved, Reason: "not found", Workflows: []string{"b.yml"}},
			{Hostname: "tenant.ghe.com", NWO: "o/r", Ref: "v1", Resolution: pin.Pinned, SHA: "abc", Workflows: []string{"c.yml"}},
			{NWO: "o/r", Ref: "V1", Resolution: pin.Pinned, SHA: "def", Workflows: []string{"d.yml"}},
		}}

		path := writeRunLog(dir, report, record, false, "v0.0.3", "github.com", "github.com/o/r", errors.New("planning pins: boom"))
		require.NotEmpty(t, path)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		var payload struct {
			Repo      string           `json:"repo"`
			Succeeded bool             `json:"succeeded"`
			Error     string           `json:"error"`
			Valid     bool             `json:"valid"`
			Findings  []map[string]any `json:"findings"`
			Pins      []map[string]any `json:"pins"`
		}
		require.NoError(t, json.Unmarshal(b, &payload))
		assert.Equal(t, "github.com/o/r", payload.Repo)
		assert.False(t, payload.Succeeded)
		assert.Equal(t, "planning pins: boom", payload.Error)
		assert.False(t, payload.Valid)
		require.Len(t, payload.Findings, 1)
		assert.Equal(t, "local path cannot be resolved", payload.Findings[0]["detail"])
		require.Len(t, payload.Pins, 3, "dedupe by host/NWO@Ref; host and NWO fold case, ref does not")
		assert.Equal(t, "unresolved", payload.Pins[0]["outcome"])
		assert.Equal(t, "not found", payload.Pins[0]["reason"])
		assert.Equal(t, "tenant.ghe.com", payload.Pins[1]["hostname"])
		assert.Equal(t, "V1", payload.Pins[2]["ref"])
	})

	t.Run("keeps the retention count including the new log", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < runLogRetentionCount; i++ {
			name := filepath.Join(dir, fmt.Sprintf("old-%03d.json", i))
			require.NoError(t, os.WriteFile(name, nil, 0o600))
			mtime := time.Now().Add(-time.Duration(i+1) * time.Minute)
			require.NoError(t, os.Chtimes(name, mtime, mtime))
		}

		path := writeRunLog(dir, &checks.Report{}, nil, true, "", "github.com", "", nil)

		require.NotEmpty(t, path)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, runLogRetentionCount)
		assert.FileExists(t, path)
	})

	t.Run("returns empty path when dir is unusable", func(t *testing.T) {
		assert.Empty(t, writeRunLog("", &checks.Report{}, nil, true, "", "github.com", "", nil))
	})
}

func TestGcLogs(t *testing.T) {
	t.Run("removes files older than retention age", func(t *testing.T) {
		dir := t.TempDir()
		oldFile := filepath.Join(dir, "old.json")
		require.NoError(t, os.WriteFile(oldFile, []byte("old"), 0o644))
		oldTime := time.Now().Add(-15 * 24 * time.Hour)
		require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))
		newFile := filepath.Join(dir, "new.json")
		require.NoError(t, os.WriteFile(newFile, []byte("new"), 0o644))

		gcLogs(dir)

		assert.NoFileExists(t, oldFile)
		assert.FileExists(t, newFile)
	})

	t.Run("retains at most the retention count", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < runLogRetentionCount+5; i++ {
			name := filepath.Join(dir, fmt.Sprintf("run-%03d.json", i))
			require.NoError(t, os.WriteFile(name, []byte("data"), 0o644))
			mtime := time.Now().Add(-time.Duration(i) * time.Minute)
			require.NoError(t, os.Chtimes(name, mtime, mtime))
		}

		gcLogs(dir)

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, runLogRetentionCount)
	})

	t.Run("leaves directories alone", func(t *testing.T) {
		dir := t.TempDir()
		subdir := filepath.Join(dir, "subdir")
		require.NoError(t, os.Mkdir(subdir, 0o755))

		gcLogs(dir)

		assert.DirExists(t, subdir)
	})
}

// Regression for github-early-access/actions-locked-dependencies#59: a fix
// run rejected because a remote composite uses a local path left a record
// claiming valid with no workflows.
func TestCheck_FailedRunLogsRejectedWorkflow(t *testing.T) {
	logDir := isolateRunLogs(t)

	reg := &httpmock.Registry{}
	compositeYAML := "name: Publish\nruns:\n  using: composite\n  steps:\n    - uses: ./.github/.tmp/run-in-docker\n"
	reg.Register(
		httpmock.GraphQLForRepo("pypa", "gh-action-pypi-publish"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": testRepoResponse("pypa/gh-action-pypi-publish", "76f52bc884231f62b9a034ebfe128415bbaabdfc", compositeYAML),
			},
		}),
	)

	workflowPath := writeTempWorkflow(t, `
name: ci
on: push
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: pypa/gh-action-pypi-publish@release/v1
`)

	_, stderr, err := runCommandWithHTTP(t, reg, "--no-interactive", "--no-narrow", workflowPath)
	require.Error(t, err)
	assert.Contains(t, stderr, "Run log:")

	var payload struct {
		Succeeded bool `json:"succeeded"`
		Valid     bool `json:"valid"`
		Workflows []struct {
			Path  string `json:"path"`
			Valid bool   `json:"valid"`
		} `json:"workflows"`
		Findings []struct {
			Category string `json:"category"`
			Detail   string `json:"detail"`
		} `json:"findings"`
	}
	readOnlyRunLog(t, logDir, &payload)
	assert.False(t, payload.Succeeded)
	assert.False(t, payload.Valid)
	require.Len(t, payload.Workflows, 1)
	assert.Equal(t, workflowPath, payload.Workflows[0].Path)
	assert.False(t, payload.Workflows[0].Valid)
	require.NotEmpty(t, payload.Findings)
	assert.Equal(t, "local-action", payload.Findings[0].Category)
	assert.Contains(t, payload.Findings[0].Detail, "./.github/.tmp/run-in-docker")
}

func TestCheck_RunLogOnEveryExitPath(t *testing.T) {
	t.Run("failure before diagnosis still logs the error", func(t *testing.T) {
		logDir := isolateRunLogs(t)
		writeTempWorkflow(t, "name: ci\non: push\njobs: {}\n")

		_, stderr, err := runCommandWithHTTP(t, &httpmock.Registry{}, ".github/workflows/missing.yml")
		require.Error(t, err)
		assert.Contains(t, stderr, "Run log:")

		var payload struct {
			Succeeded bool   `json:"succeeded"`
			Valid     bool   `json:"valid"`
			Error     string `json:"error"`
		}
		readOnlyRunLog(t, logDir, &payload)
		assert.False(t, payload.Succeeded)
		assert.False(t, payload.Valid)
		assert.Equal(t, err.Error(), payload.Error)
	})

	t.Run("successful run logs success without printing the path", func(t *testing.T) {
		logDir := isolateRunLogs(t)
		reg := &httpmock.Registry{}
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
`, "actions/checkout@v6=sha1-de0fac2e4500dabe0009e67214ff5f5447ce83dd")

		_, stderr, err := runCommandWithHTTP(t, reg, "--no-narrow", workflowPath)
		require.NoError(t, err)
		assert.NotContains(t, stderr, "Run log:")

		var payload struct {
			Succeeded bool    `json:"succeeded"`
			Error     *string `json:"error"`
		}
		readOnlyRunLog(t, logDir, &payload)
		assert.True(t, payload.Succeeded)
		assert.Nil(t, payload.Error)
	})
}

func TestCheck_PanickedRunLogsFailure(t *testing.T) {
	logDir := isolateRunLogs(t)
	writeTempWorkflow(t, "name: ci\non: push\njobs: {}\n")
	cmd := newRootCmd(func(string, *pinpool.Pool) (*resolve.Resolver, error) { panic("boom") })
	cmd.SetArgs(nil)

	assert.PanicsWithValue(t, "boom", func() { _ = cmd.Execute() })

	var payload struct {
		Succeeded bool   `json:"succeeded"`
		Error     string `json:"error"`
	}
	readOnlyRunLog(t, logDir, &payload)
	assert.False(t, payload.Succeeded)
	assert.Equal(t, "panic: boom", payload.Error)
}

// isolateRunLogs points the user cache dir at a temp dir and returns the
// run log dir inside it.
func isolateRunLogs(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LocalAppData", cache)
	dir := runLogDir()
	require.NotEmpty(t, dir)
	return dir
}

func readOnlyRunLog(t *testing.T, dir string, v any) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, v))
}
