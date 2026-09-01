package pin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPinOrVerified(t *testing.T) {
	tests := []struct {
		name string
		r    Resolution
		want bool
	}{
		{"pinned", Pinned, true},
		{"verified", Verified, true},
		{"investigate", Investigate, false},
		{"skipped", Skipped, false},
		{"unresolved", Unresolved, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isPinOrVerified(tt.r))
		})
	}
}

func TestGroupPinnedByWorkflow(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v4", SHA: "aaa", Resolution: Pinned, OnBranch: "main", Tag: "v4.1.0", Workflows: []string{"ci.yml", "release.yml"}},
			{NWO: "actions/setup-go", Ref: "v5", SHA: "bbb", Resolution: Verified, Workflows: []string{"ci.yml"}},
			{NWO: "bad/action", Ref: "v1", SHA: "ccc", Resolution: Investigate, Workflows: []string{"ci.yml"}},
			{NWO: "actions/cache", Ref: "v3", SHA: "ddd", Resolution: Skipped, Workflows: []string{"release.yml"}},
		},
	}

	got := groupPinnedByWorkflow(rec)

	// Only pinned and verified entries should appear
	require.Contains(t, got, "ci.yml")
	require.Contains(t, got, "release.yml")

	ci := got["ci.yml"]
	require.Len(t, ci, 2, "ci.yml should have checkout and setup-go")
	assert.Equal(t, "actions/checkout", ci[0].NWO)
	assert.Equal(t, "aaa", ci[0].SHA)
	assert.Equal(t, "main", ci[0].Branch)
	assert.Equal(t, "v4.1.0", ci[0].Tag)
	assert.Equal(t, "actions/setup-go", ci[1].NWO)

	release := got["release.yml"]
	require.Len(t, release, 1, "release.yml should have only checkout")
	assert.Equal(t, "actions/checkout", release[0].NWO)
}

func TestGroupPinnedByWorkflow_empty(t *testing.T) {
	rec := &Record{}
	got := groupPinnedByWorkflow(rec)
	assert.Empty(t, got)
}

func TestBuildParentMap(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"ci.yml", "release.yml"}, RequiredBy: []string{"parent/action@v2"}},
			{NWO: "c/d", Ref: "v2", Resolution: Verified, Workflows: []string{"ci.yml"}},
			{NWO: "e/f", Ref: "v3", Resolution: Investigate, Workflows: []string{"ci.yml"}, RequiredBy: []string{"x/y@v1"}},
		},
	}

	pm := buildParentMap(rec, "ci.yml")
	assert.Equal(t, []string{"parent/action@v2"}, pm["a/b@v1"])
	assert.NotContains(t, pm, "c/d@v2", "entries without RequiredBy should not appear")
	assert.NotContains(t, pm, "e/f@v3", "investigate entries should be excluded")
}

func TestBuildParentMap_filters_workflow(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Workflows: []string{"release.yml"}, RequiredBy: []string{"p"}},
		},
	}
	pm := buildParentMap(rec, "ci.yml")
	assert.Empty(t, pm, "should not include entries from other workflows")
}

func TestBuildDirectKeys(t *testing.T) {
	rec := &Record{
		Entries: []Entry{
			{NWO: "a/b", Ref: "v1", Resolution: Pinned, Direct: true, Workflows: []string{"ci.yml"}},
			{NWO: "c/d", Ref: "v2", Resolution: Verified, Direct: false, Workflows: []string{"ci.yml"}},
			{NWO: "e/f", Ref: "v3", Resolution: Pinned, Direct: true, Workflows: []string{"release.yml"}},
			{NWO: "g/h", Ref: "v4", Resolution: Investigate, Direct: true, Workflows: []string{"ci.yml"}},
		},
	}

	keys := buildDirectKeys(rec, "ci.yml")
	assert.True(t, keys["a/b@v1"])
	assert.NotContains(t, keys, "c/d@v2", "non-direct should be excluded")
	assert.NotContains(t, keys, "e/f@v3", "wrong workflow should be excluded")
	assert.NotContains(t, keys, "g/h@v4", "investigate should be excluded")
}

func TestCommitRemovesDependenciesDroppedFromWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	workflow := []byte(`on: push
jobs:
  lint:
    uses: owner/reusable/.github/workflows/lint.yml@main
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), workflow, 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	oldParent := dep.Dependency{NWO: "owner/action", Ref: "main", SHA: strings.Repeat("1", 40), HashAlgo: "sha1"}
	oldChild := dep.Dependency{NWO: "actions/setup-go", Ref: "v5", SHA: strings.Repeat("2", 40), HashAlgo: "sha1"}
	keep := dep.Dependency{NWO: "actions/checkout", Ref: "v7", SHA: strings.Repeat("3", 40), HashAlgo: "sha1"}
	require.NoError(t, store.Set(context.Background(), workflowPath,
		[]dep.Dependency{oldParent, oldChild, keep},
		map[string][]string{oldChild.Key(): {oldParent.Key()}},
		map[string]bool{oldParent.Key(): true, keep.Key(): true}))
	require.NoError(t, store.Save())

	rec := &Record{
		Entries: []Entry{{
			NWO:        keep.NWO,
			Ref:        keep.Ref,
			SHA:        keep.SHA,
			Resolution: Verified,
			Direct:     true,
			Workflows:  []string{workflowPath},
		}},
		Workflows: []WorkflowPlan{{Path: workflowPath}},
	}
	require.NoError(t, Commit(context.Background(), rec, store, nil))

	gotWorkflow, err := os.ReadFile(filepath.Join(dir, workflowPath))
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)

	got, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "actions/checkout@v7")
	assert.NotContains(t, string(got), "owner/action@main")
	assert.NotContains(t, string(got), "actions/setup-go@v5")
}

func TestCommitScopedParentAdvanceKeepsUntouchedWorkflowEdges(t *testing.T) {
	dir := t.TempDir()
	ciPath := filepath.Join(".github", "workflows", "ci.yml")
	releasePath := filepath.Join(".github", "workflows", "release.yml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ciPath), []byte("on: push\njobs: {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, releasePath), []byte("on: push\njobs: {}\n"), 0o644))
	t.Chdir(dir)

	ctx := context.Background()
	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	parent := dep.Dependency{NWO: "owner/composite", Ref: "main", SHA: strings.Repeat("1", 40), HashAlgo: "sha1"}
	releaseChild := dep.Dependency{NWO: "owner/release-child", Ref: "v1", SHA: strings.Repeat("2", 40), HashAlgo: "sha1"}
	require.NoError(t, store.Set(ctx, ciPath,
		[]dep.Dependency{parent}, nil, map[string]bool{parent.Key(): true}))
	require.NoError(t, store.Set(ctx, releasePath,
		[]dep.Dependency{parent, releaseChild},
		map[string][]string{releaseChild.Key(): {parent.Key()}},
		map[string]bool{parent.Key(): true}))
	require.NoError(t, store.Save())

	newParentSHA := strings.Repeat("3", 40)
	ciChildSHA := strings.Repeat("4", 40)
	rec := &Record{
		Entries: []Entry{
			{NWO: parent.NWO, Ref: parent.Ref, SHA: newParentSHA, Resolution: Pinned, Direct: true, Workflows: []string{ciPath}},
			{NWO: "owner/ci-child", Ref: "v2", SHA: ciChildSHA, Resolution: Pinned, RequiredBy: []string{parent.Key()}, Workflows: []string{ciPath}},
		},
		Workflows: []WorkflowPlan{{Path: ciPath}},
	}
	require.NoError(t, Commit(ctx, rec, store, nil))

	file := store.File()
	action := file.Dependencies["owner/composite@main"]
	assert.Equal(t, "sha1-"+newParentSHA, action.Commit)
	assert.Equal(t, []string{"owner/ci-child@v2", "owner/release-child@v1"}, action.Uses)
	assert.Contains(t, file.Dependencies, "owner/release-child@v1")
}

func TestCommitConflictingScopedPlansKeepRecordedSharedClosure(t *testing.T) {
	for _, workflows := range [][]string{
		{".github/workflows/ci.yml", ".github/workflows/release.yml"},
		{".github/workflows/release.yml", ".github/workflows/ci.yml"},
	} {
		t.Run(strings.Join(workflows, "_then_"), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
			for _, path := range workflows {
				require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte("on: push\njobs: {}\n"), 0o644))
			}
			t.Chdir(dir)

			ctx := context.Background()
			store, err := lockfile.LoadState(dir, fakeMeta{})
			require.NoError(t, err)
			oldParent := dep.Dependency{NWO: "owner/composite", Ref: "main", SHA: strings.Repeat("1", 40), HashAlgo: "sha1"}
			oldChild := dep.Dependency{NWO: "owner/old-child", Ref: "v1", SHA: strings.Repeat("2", 40), HashAlgo: "sha1"}
			for _, path := range workflows {
				require.NoError(t, store.Set(ctx, path,
					[]dep.Dependency{oldParent, oldChild},
					dep.ParentMap{oldChild.Key(): {oldParent.Key()}},
					map[string]bool{oldParent.Key(): true}))
			}
			require.NoError(t, store.Save())

			newParentSHA := strings.Repeat("3", 40)
			newChild := dep.Dependency{NWO: "owner/new-child", Ref: "v2", SHA: strings.Repeat("4", 40), HashAlgo: "sha1"}
			entriesByWorkflow := map[string][]Entry{
				".github/workflows/ci.yml": {
					{NWO: oldParent.NWO, Ref: oldParent.Ref, SHA: newParentSHA, Resolution: Pinned, Direct: true, Workflows: []string{".github/workflows/ci.yml"}},
					{NWO: newChild.NWO, Ref: newChild.Ref, SHA: newChild.SHA, Resolution: Pinned, RequiredBy: []string{oldParent.Key()}, Workflows: []string{".github/workflows/ci.yml"}},
				},
				".github/workflows/release.yml": {
					{NWO: oldParent.NWO, Ref: oldParent.Ref, SHA: oldParent.SHA, Resolution: Verified, Direct: true, Workflows: []string{".github/workflows/release.yml"}},
					{NWO: oldChild.NWO, Ref: oldChild.Ref, SHA: oldChild.SHA, Resolution: Verified, RequiredBy: []string{oldParent.Key()}, Workflows: []string{".github/workflows/release.yml"}},
				},
			}
			rec := &Record{}
			for _, path := range workflows {
				rec.Entries = append(rec.Entries, entriesByWorkflow[path]...)
				rec.Workflows = append(rec.Workflows, WorkflowPlan{Path: path})
			}

			require.NoError(t, Commit(ctx, rec, store, nil))

			file := store.File()
			action := file.Dependencies[oldParent.Key()]
			assert.Equal(t, "sha1-"+oldParent.SHA, action.Commit)
			assert.Equal(t, []string{oldChild.Key()}, action.Uses)
			assert.Contains(t, file.Dependencies, oldChild.Key())
			assert.NotContains(t, file.Dependencies, newChild.Key())
		})
	}
}
