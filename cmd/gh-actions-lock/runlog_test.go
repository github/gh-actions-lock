package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/github/gh-actions-lock/internal/pin"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
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

		path := writeRunLog(dir, report, record, false, "v0.0.3", "github.com", "github.com/o/r")
		require.NotEmpty(t, path)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		var payload struct {
			Repo     string           `json:"repo"`
			Valid    bool             `json:"valid"`
			Findings []map[string]any `json:"findings"`
			Pins     []map[string]any `json:"pins"`
		}
		require.NoError(t, json.Unmarshal(b, &payload))
		assert.Equal(t, "github.com/o/r", payload.Repo)
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

		path := writeRunLog(dir, &checks.Report{}, nil, true, "", "github.com", "")

		require.NotEmpty(t, path)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, runLogRetentionCount)
		assert.FileExists(t, path)
	})

	t.Run("returns empty path when dir is unusable", func(t *testing.T) {
		assert.Empty(t, writeRunLog("", &checks.Report{}, nil, true, "", "github.com", ""))
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
