package pin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecord_Pinned(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v4", Resolution: Pinned},
			{NWO: "actions/setup-go", Ref: "v5", Resolution: Verified},
			{NWO: "actions/cache", Ref: "v3", Resolution: Pinned},
			{NWO: "owner/bad", Ref: "v1", Resolution: Investigate},
		},
	}
	got := rec.Pinned()
	require.Len(t, got, 2)
	assert.Equal(t, "actions/checkout", got[0].NWO)
	assert.Equal(t, "actions/cache", got[1].NWO)
}

func TestRecord_Investigated(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned},
			{NWO: "c/d", Ref: "v2", Resolution: Investigate},
		},
	}
	got := rec.Investigated()
	require.Len(t, got, 1)
	assert.Equal(t, "c/d", got[0].NWO)
}

func TestRecord_Pinned_empty(t *testing.T) {
	rec := &Record{}
	assert.Empty(t, rec.Pinned())
}

func TestRecord_Valid(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry
		want    bool
	}{
		{"empty record is valid", nil, true},
		{"all pinned is valid", []Entry{
			{Resolution: Pinned}, {Resolution: Verified},
		}, true},
		{"investigate makes invalid", []Entry{
			{Resolution: Pinned}, {Resolution: Investigate},
		}, false},
		{"unresolved makes invalid", []Entry{
			{Resolution: Unresolved},
		}, false},
		{"skipped is valid", []Entry{
			{Resolution: Skipped},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &Record{Entries: tt.entries}
			assert.Equal(t, tt.want, rec.Valid())
		})
	}
}

func TestAppendUnique(t *testing.T) {
	tests := []struct {
		name  string
		base  []string
		items []string
		want  []string
	}{
		{"nil base", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"no duplicates", []string{"a"}, []string{"b"}, []string{"a", "b"}},
		{"deduplicates", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"all duplicates", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
		{"empty items", []string{"a"}, nil, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendUnique(tt.base, tt.items...)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDedupActions(t *testing.T) {
	t.Run("merges entries sharing NWO@Ref", func(t *testing.T) {
		entries := []Entry{
			{NWO: "actions/checkout", Ref: "v4", SHA: "abc", Workflows: []string{"ci.yml"}},
			{NWO: "actions/checkout", Ref: "v4", SHA: "abc", Workflows: []string{"release.yml"}},
			{NWO: "actions/setup-go", Ref: "v5", SHA: "def", Workflows: []string{"ci.yml"}},
		}
		got := dedupActions(entries)
		require.Len(t, got, 2)
		assert.Equal(t, "actions/checkout", got[0].NWO)
		assert.Equal(t, []string{"ci.yml", "release.yml"}, got[0].Workflows)
		assert.Equal(t, "actions/setup-go", got[1].NWO)
	})

	t.Run("no duplicates returns same entries", func(t *testing.T) {
		entries := []Entry{
			{NWO: "a/b", Ref: "v1", Workflows: []string{"w1"}},
			{NWO: "c/d", Ref: "v2", Workflows: []string{"w2"}},
		}
		got := dedupActions(entries)
		require.Len(t, got, 2)
	})

	t.Run("empty input", func(t *testing.T) {
		assert.Empty(t, dedupActions(nil))
	})

	t.Run("same NWO different ref kept separate", func(t *testing.T) {
		entries := []Entry{
			{NWO: "actions/checkout", Ref: "v3", Workflows: []string{"a"}},
			{NWO: "actions/checkout", Ref: "v4", Workflows: []string{"b"}},
		}
		got := dedupActions(entries)
		require.Len(t, got, 2)
	})

	t.Run("workflow dedup within merge", func(t *testing.T) {
		entries := []Entry{
			{NWO: "a/b", Ref: "v1", Workflows: []string{"ci.yml", "deploy.yml"}},
			{NWO: "a/b", Ref: "v1", Workflows: []string{"ci.yml", "test.yml"}},
		}
		got := dedupActions(entries)
		require.Len(t, got, 1)
		assert.Equal(t, []string{"ci.yml", "deploy.yml", "test.yml"}, got[0].Workflows)
	})
}

func TestRecord_Summary(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"ci.yml"}},
			{NWO: "c/d", Ref: "v2", Resolution: Verified, Workflows: []string{"ci.yml", "release.yml"}},
			{NWO: "e/f", Ref: "v3", Resolution: Investigate, Workflows: []string{"release.yml"}},
			{NWO: "g/h", Ref: "v4", Resolution: Skipped, Workflows: []string{"ci.yml"}, FullScan: true},
			{NWO: "i/j", Ref: "v5", Resolution: Unresolved, Workflows: []string{"ci.yml"}},
		},
	}
	// summary() takes the deduped action list as argument
	actions := dedupActions(rec.Entries)
	s := rec.summary(actions)

	assert.Equal(t, 2, s.Workflows) // ci.yml and release.yml
	assert.Equal(t, 5, s.Actions)
	assert.False(t, s.Valid) // has investigate + unresolved
	assert.Equal(t, 1, s.Pinned)
	assert.Equal(t, 1, s.AlreadyPinned)
	assert.Equal(t, 1, s.Investigate)
	assert.Equal(t, 1, s.Skipped)
	assert.Equal(t, 1, s.Unresolved)
	assert.Equal(t, 1, s.FullScan)
}

func TestRecord_Summary_valid(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"ci.yml"}},
		},
	}
	s := rec.summary(rec.Entries)
	assert.True(t, s.Valid)
}

func TestRecord_MarshalJSON(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	rec := &Record{
		Version: "1.2.3",
		Created: now,
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v4", SHA: "abc123", Resolution: Pinned, Workflows: []string{"ci.yml"}, Direct: true},
		},
		Repo: &RepoInfo{Owner: "myorg", Name: "myrepo", Host: "github.com"},
	}

	b, err := json.Marshal(rec)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &raw))

	// Check schema and generated_at
	var schema string
	require.NoError(t, json.Unmarshal(raw["schema"], &schema))
	assert.Equal(t, "run-record/v1", schema)

	var generatedAt string
	require.NoError(t, json.Unmarshal(raw["generated_at"], &generatedAt))
	assert.Equal(t, "2025-06-01T12:00:00Z", generatedAt)

	// Check tool info
	var tool toolInfo
	require.NoError(t, json.Unmarshal(raw["tool"], &tool))
	assert.Equal(t, "gh-actions-lock", tool.Name)
	assert.Equal(t, "1.2.3", tool.Version)

	// Check actions are present
	var actions []Entry
	require.NoError(t, json.Unmarshal(raw["actions"], &actions))
	require.Len(t, actions, 1)
	assert.Equal(t, "actions/checkout", actions[0].NWO)

	// Check summary
	var summary Summary
	require.NoError(t, json.Unmarshal(raw["summary"], &summary))
	assert.Equal(t, 1, summary.Pinned)
	assert.True(t, summary.Valid)

	// Check repo
	var repo RepoInfo
	require.NoError(t, json.Unmarshal(raw["repo"], &repo))
	assert.Equal(t, "myorg", repo.Owner)
}

func TestRecord_MarshalJSON_deduplicates(t *testing.T) {
	rec := &Record{
		Created: time.Now(),
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", SHA: "aaa", Resolution: Pinned, Workflows: []string{"w1"}},
			{NWO: "a/b", Ref: "v1", SHA: "aaa", Resolution: Pinned, Workflows: []string{"w2"}},
		},
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &raw))

	var actions []Entry
	require.NoError(t, json.Unmarshal(raw["actions"], &actions))
	require.Len(t, actions, 1, "should deduplicate entries with same NWO@Ref")
	assert.Equal(t, []string{"w1", "w2"}, actions[0].Workflows)
}

func TestGcLogs(t *testing.T) {
	t.Run("removes files older than retention age", func(t *testing.T) {
		dir := t.TempDir()

		// Create an old file (set mtime in the past)
		oldFile := filepath.Join(dir, "old.json")
		require.NoError(t, os.WriteFile(oldFile, []byte("old"), 0o644))
		oldTime := time.Now().Add(-15 * 24 * time.Hour) // 15 days ago
		require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))

		// Create a recent file
		newFile := filepath.Join(dir, "new.json")
		require.NoError(t, os.WriteFile(newFile, []byte("new"), 0o644))

		gcLogs(dir)

		assert.NoFileExists(t, oldFile, "old file should be removed")
		assert.FileExists(t, newFile, "recent file should remain")
	})

	t.Run("retains at most retentionCount files", func(t *testing.T) {
		dir := t.TempDir()

		// Create more files than retention count
		for i := 0; i < 55; i++ {
			name := filepath.Join(dir, fmt.Sprintf("run-%03d.json", i))
			require.NoError(t, os.WriteFile(name, []byte("data"), 0o644))
			// Stagger mod times so sort is deterministic
			mtime := time.Now().Add(-time.Duration(i) * time.Minute)
			require.NoError(t, os.Chtimes(name, mtime, mtime))
		}

		gcLogs(dir)

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Equal(t, 50, len(entries), "should keep exactly retentionCount files")
	})

	t.Run("skips directories", func(t *testing.T) {
		dir := t.TempDir()
		subdir := filepath.Join(dir, "subdir")
		require.NoError(t, os.Mkdir(subdir, 0o755))

		gcLogs(dir)

		assert.DirExists(t, subdir, "directories should be left alone")
	})

	t.Run("nonexistent directory is a noop", func(t *testing.T) {
		gcLogs(filepath.Join(t.TempDir(), "nonexistent"))
		// Should not panic
	})
}
