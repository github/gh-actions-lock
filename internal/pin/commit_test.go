package pin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/workflowfile"
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

func TestCommitDependencyFreeWorkflow(t *testing.T) {
	tests := []struct {
		name                   string
		skipNewWorkflowEntries bool
		wantEntry              bool
	}{
		{"records empty entry", false, true},
		{"skips onboarding", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			workflowPath := filepath.Join(".github", "workflows", "ci.yml")
			require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte("on: push\n"), 0o644))
			t.Chdir(dir)

			store, err := lockfile.LoadState(dir, fakeMeta{})
			require.NoError(t, err)
			rec := &Record{Workflows: []WorkflowPlan{{Path: workflowPath}}}

			require.NoError(t, Commit(context.Background(), rec, store, &CommitOptions{SkipNewWorkflowEntries: tt.skipNewWorkflowEntries}))
			assert.Equal(t, tt.wantEntry, store.HasWorkflow(workflowfile.KeyFromPath(workflowPath)))
		})
	}
}

func TestCommitPartialResolution(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte("on: push\n"), 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{
		Entries: []Entry{{
			NWO: "actions/checkout", Ref: "v4", SHA: strings.Repeat("a", 40),
			Resolution: Pinned, Workflows: []string{workflowPath}, Direct: true,
		}},
		Workflows: []WorkflowPlan{{Path: workflowPath}},
	}

	require.NoError(t, Commit(context.Background(), rec, store, nil))
	deps, err := store.Get(workflowfile.KeyFromPath(workflowPath))
	require.NoError(t, err)
	assert.Len(t, deps, 1)
}

func TestCommitSkipNewWorkflowEntriesPreventsRewrites(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	actionPath := filepath.Join(".github", "actions", "local", "action.yml")
	oldUses := "actions/checkout@" + strings.Repeat("a", 40)
	newUses := "actions/checkout@v4.2.0"
	workflowContent := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + oldUses + "\n")
	actionContent := []byte("name: local\nruns:\n  using: composite\n  steps:\n    - uses: " + oldUses + "\n")
	for path, content := range map[string][]byte{workflowPath: workflowContent, actionPath: actionContent} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), content, 0o644))
	}
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{Workflows: []WorkflowPlan{{
		Path:            workflowPath,
		Rewrites:        map[string]string{oldUses: newUses},
		SelfActionFiles: []string{actionPath},
		// A skipped workflow's required rewrites must not fail the run.
		RequiredRewrites: map[string]string{"gone/action@v1": "new/action@v1"},
	}}}

	require.NoError(t, Commit(context.Background(), rec, store, &CommitOptions{SkipNewWorkflowEntries: true}))
	workflowAfter, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	actionAfter, err := os.ReadFile(actionPath)
	require.NoError(t, err)
	assert.Equal(t, workflowContent, workflowAfter)
	assert.Equal(t, actionContent, actionAfter)
	assert.False(t, store.HasWorkflow(workflowfile.KeyFromPath(workflowPath)))
}

func TestValidateRequiredRewritesRecordsFiles(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	actionPath := filepath.Join("actions", "a", "action.yml")
	files := map[string]string{
		workflowPath: "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/actions/a\n",
		actionPath:   "name: a\nruns:\n  using: composite\n  steps:\n    - uses: old/action@v1\n",
	}
	for path, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
	}
	t.Chdir(dir)

	plans := []WorkflowPlan{{
		Path:             workflowPath,
		SelfActionFiles:  []string{actionPath},
		RequiredRewrites: map[string]string{"old/action@v1": "new/action@v1"},
	}}
	require.NoError(t, validateRequiredRewrites(plans))
	assert.Equal(t, []string{actionPath}, plans[0].RewrittenIn["old/action@v1"])
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
	require.NoError(t, store.Set(context.Background(), workflowfile.KeyFromPath(workflowPath),
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

func TestCommitRekeysAnnotatedTagObjectWithUses(t *testing.T) {
	const tagObjectSHA = "1111111111111111111111111111111111111111"
	const commitSHA = "2222222222222222222222222222222222222222"
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), []byte(`on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/action@`+tagObjectSHA+`
`), 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	parent := dep.Dependency{
		NWO: "owner/action", Ref: tagObjectSHA, SHA: tagObjectSHA,
		HashAlgo: "sha1", Tag: "v3.2.0",
	}
	child := dep.Dependency{
		NWO: "actions/checkout", Ref: "v4", SHA: strings.Repeat("3", 40),
		HashAlgo: "sha1",
	}
	require.NoError(t, store.Set(context.Background(), workflowfile.KeyFromPath(workflowPath),
		[]dep.Dependency{parent, child},
		map[string][]string{child.Key(): {parent.Key()}},
		map[string]bool{parent.Key(): true}))
	require.NoError(t, store.Save())

	rec := &Record{
		Entries: []Entry{{
			NWO:          parent.NWO,
			Ref:          "v3.2.0",
			SHA:          commitSHA,
			Resolution:   Verified,
			AutoFixedRef: tagObjectSHA,
			Direct:       true,
			Workflows:    []string{workflowPath},
			FromLock:     true,
		}},
		Workflows: []WorkflowPlan{{
			Path: workflowPath,
			Rewrites: map[string]string{
				parent.NWO + "@" + tagObjectSHA: parent.NWO + "@v3.2.0",
			},
		}},
	}
	require.NoError(t, Commit(context.Background(), rec, store, nil))

	got, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "actions.lock"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "'owner/action@v3.2.0'")
	assert.Contains(t, string(got), "uses:\n            - 'actions/checkout@v4'")
	assert.NotContains(t, string(got), "'owner/action@"+tagObjectSHA+"'")
}

// TestCommitRejectedEntryRefWritesNothing: a planned entry the lockfile
// refuses must fail before any workflow is rewritten.
func TestCommitRejectedEntryRefWritesNothing(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(".github", "workflows", "ci.yml")
	sha := strings.Repeat("a", 40)
	content := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@" + sha + "\n")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(workflowPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workflowPath), content, 0o644))
	t.Chdir(dir)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)
	rec := &Record{
		Entries: []Entry{{
			NWO: "actions/checkout", Ref: "release-g1234", SHA: sha, Tag: "release-g1234",
			Resolution: Pinned, Workflows: []string{workflowPath}, Direct: true,
		}},
		Workflows: []WorkflowPlan{{
			Path:     workflowPath,
			Rewrites: map[string]string{"actions/checkout@" + sha: "actions/checkout@release-g1234"},
		}},
	}

	err = Commit(context.Background(), rec, store, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release-g1234")
	after, err := os.ReadFile(filepath.Join(dir, workflowPath))
	require.NoError(t, err)
	assert.Equal(t, string(content), string(after), "the workflow is not rewritten")
	assert.NoFileExists(t, filepath.Join(dir, ".github", "workflows", "actions.lock"))
}

// TestUpdateLockfileRepairsOnlyForWriters: an entry repair applies only when
// a workflow it was planned for is written, so a preview without that
// workflow (Audit dropping a writer) leaves the entry as recorded.
func TestUpdateLockfileRepairsOnlyForWriters(t *testing.T) {
	sha := strings.Repeat("a", 40)
	key := "actions/cache@" + sha
	for _, written := range []bool{true, false} {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
		lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/a.yml':\n        - 'actions/checkout@v4'\n" +
			"dependencies:\n    '" + key + "':\n        ref: '" + sha + "'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n" +
			"    'actions/checkout@v4':\n        ref: 'v4'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n        uses:\n            - '" + key + "'\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".github", "workflows", "actions.lock"), []byte(lock), 0o600))
		store, err := lockfile.LoadState(dir, fakeMeta{})
		require.NoError(t, err)
		wf := filepath.Join(".github", "workflows", "a.yml")
		rec := &Record{EntryRepairs: []EntryRepair{{Key: key, Ref: "v4.2.0", Workflows: []string{wf}}}}
		var writers []WorkflowPlan
		if written {
			writers = []WorkflowPlan{{Path: filepath.Join(".github", "workflows", "b.yml")}, {Path: wf}}
		}
		preview := store.Preview()
		require.NoError(t, updateLockfile(context.Background(), rec, writers, preview))
		a, ok := preview.Entry(key)
		require.True(t, ok)
		want := sha
		if written {
			want = "v4.2.0"
		}
		assert.Equal(t, want, a.Ref, "written=%v", written)
	}
}

// TestCommitRefusesIncompleteLockGraph: a.yml is refreshed and adds a use
// of the composite b.yml already locks, whose recorded child has a missing
// descendant. The write would leave a.yml's closure incomplete, so nothing
// is written.
func TestCommitRefusesIncompleteLockGraph(t *testing.T) {
	sha := strings.Repeat("a", 40)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	entry := func(key, uses string) string {
		e := "    '" + key + "':\n        ref: 'v1'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n"
		if uses != "" {
			e += "        uses:\n            - '" + uses + "'\n"
		}
		return e
	}
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/a.yml':\n        - 'o/leaf@v1'\n    '.github/workflows/b.yml':\n        - 'o/comp@v1'\n" +
		"dependencies:\n" + entry("o/comp@v1", "o/child@v1") + entry("o/child@v1", "o/gone@v1") + entry("o/leaf@v1", "")
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	wf := filepath.Join(".github", "workflows", "a.yml")
	require.NoError(t, os.WriteFile(filepath.Join(dir, wf), []byte("on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: o/comp@v1\n      - uses: o/leaf@v1\n"), 0o644))
	t.Chdir(dir)
	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	rec := &Record{
		Entries: []Entry{
			{NWO: "o/comp", Ref: "v1", SHA: sha, Tag: "v1", Resolution: Verified, Workflows: []string{wf}, Direct: true, FromLock: true},
			{NWO: "o/leaf", Ref: "v1", SHA: sha, Tag: "v1", Resolution: Verified, Workflows: []string{wf}, Direct: true, FromLock: true},
		},
		Workflows: []WorkflowPlan{{Path: wf}},
	}
	err = Commit(context.Background(), rec, store, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "o/child@v1 uses o/gone@v1, which has no dependencies entry")
	assert.Contains(t, err.Error(), "gh actions-lock update .github/workflows/a.yml")
	after, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	assert.Equal(t, lock, string(after), "nothing is written")
}

// TestCommitKeepsEdgesUnwrittenWorkflowsNeed: a.yml uses o/actions/one and
// unselected b.yml uses o/actions/two, which share the o/actions@v1 entry.
// Writing a.yml from a read of /one alone must keep /two's child.
func TestCommitKeepsEdgesUnwrittenWorkflowsNeed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	entry := func(key, uses string) string {
		e := "    '" + key + "':\n        ref: 'v1'\n        commit: 'sha1-" + sha + "'\n        owner_id: 1\n        repo_id: 1\n"
		if uses != "" {
			e += "        uses:\n" + uses
		}
		return e
	}
	lock := "version: 'v0.0.3'\nworkflows:\n    '.github/workflows/a.yml':\n        - 'o/actions@v1'\n    '.github/workflows/b.yml':\n        - 'o/actions@v1'\n" +
		"dependencies:\n" + entry("o/actions@v1", "            - 'o/one-child@v1'\n            - 'o/two-child@v1'\n") +
		entry("o/one-child@v1", "") + entry("o/two-child@v1", "")
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(lock), 0o600))
	wf := filepath.Join(".github", "workflows", "a.yml")
	require.NoError(t, os.WriteFile(filepath.Join(dir, wf), []byte("on: push\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: o/actions/one@v1\n"), 0o644))
	t.Chdir(dir)
	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	rec := &Record{
		Entries: []Entry{
			{NWO: "o/actions", Ref: "v1", SHA: sha, Tag: "v1", Resolution: Verified, Workflows: []string{wf}, Direct: true},
			{NWO: "o/one-child", Ref: "v1", SHA: sha, Tag: "v1", Resolution: Verified, Workflows: []string{wf}, RequiredBy: []string{"o/actions@v1"}},
		},
		Workflows: []WorkflowPlan{{Path: wf}},
	}
	require.NoError(t, Commit(context.Background(), rec, store, nil))
	a, ok := store.File().Dependencies["o/actions@v1"]
	require.True(t, ok)
	assert.Equal(t, []string{"o/one-child@v1", "o/two-child@v1"}, a.Uses)
	assert.Contains(t, store.File().Dependencies, "o/two-child@v1", "b.yml's child survives GC")
}
