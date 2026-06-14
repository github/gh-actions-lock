package pin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/workflowfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeMeta struct{}

func (fakeMeta) RepoIDs(_ context.Context, _, _ string) (int64, int64, error) {
	return 1, 2, nil
}

// A co-located bump forces a workflow rewrite; the impostor pin already on
// disk must be retained, not silently dropped toward an empty pin list.
func TestRetainUnresolvablePins_keepsExistingPinOnColocatedRepin(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	wfKey := workflowfile.KeyFromPath(wfPath)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	seed := []dep.Dependency{
		{NWO: "bad/impostor", Ref: "v1", Tag: "v1", Branch: "main", SHA: "1111111111111111111111111111111111111111", HashAlgo: "sha1"},
		{NWO: "actions/checkout", Ref: "v4", Tag: "v4", Branch: "main", SHA: "2222222222222222222222222222222222222222", HashAlgo: "sha1"},
	}
	require.NoError(t, store.Set(context.Background(), wfKey, seed, nil, nil))
	require.NoError(t, store.Save())

	// Re-pin: checkout bumps (Pinned), impostor flagged Investigate and dropped from deps.
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v5", SHA: "3333333333333333333333333333333333333333", Resolution: Pinned, Direct: true, OnBranch: "main", Workflows: []string{wfPath}},
			{NWO: "bad/impostor", Ref: "v1", SHA: "1111111111111111111111111111111111111111", Resolution: Investigate, Issue: string(checks.ImpostorCommit), Workflows: []string{wfPath}},
		},
	}
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v5", Branch: "main", SHA: "3333333333333333333333333333333333333333", HashAlgo: "sha1"},
	}
	directKeys := map[string]bool{"actions/checkout@v5": true}

	got := retainUnresolvablePins(rec, store, wfPath, deps, directKeys)

	require.Len(t, got, 2, "impostor pin must be re-added alongside the bumped pin")
	assert.True(t, directKeys["bad/impostor@v1"], "retained impostor pin must stay direct")

	// Drive the write as Commit does; the impostor pin must survive on disk.
	require.NoError(t, store.Set(context.Background(), wfKey, got, buildParentMap(rec, wfPath), directKeys))
	require.NoError(t, store.Save())

	after, err := store.Get(wfKey)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, d := range after {
		names[d.NWO] = true
	}
	assert.True(t, names["bad/impostor"], "impostor pin must survive the re-pin write")
	assert.True(t, names["actions/checkout"], "bumped pin must be written")
	assert.Len(t, after, 2, "pin list must not shrink")
}

// With no co-located new pin, the impostor's workflow is untouched and there
// is nothing to retain; the helper is a no-op on the deps it is handed.
func TestRetainUnresolvablePins_noopWithoutImpostorFinding(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfPath := filepath.Join(dir, ".github", "workflows", "ci.yml")

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	rec := &Record{Entries: []Entry{
		{NWO: "actions/checkout", Ref: "v5", Resolution: Pinned, Workflows: []string{wfPath}},
	}}
	deps := []dep.Dependency{{NWO: "actions/checkout", Ref: "v5"}}
	directKeys := map[string]bool{"actions/checkout@v5": true}

	got := retainUnresolvablePins(rec, store, wfPath, deps, directKeys)
	assert.Len(t, got, 1)
}

// Unresolved entries (403, transient errors) should also be retained so
// a co-located re-pin doesn't silently drop the existing pin.
func TestRetainUnresolvablePins_keepsUnresolvedPin(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	wfPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	wfKey := workflowfile.KeyFromPath(wfPath)

	store, err := lockfile.LoadState(dir, fakeMeta{})
	require.NoError(t, err)

	seed := []dep.Dependency{
		{NWO: "corp/private", Ref: "v2", Tag: "v2", Branch: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HashAlgo: "sha1"},
		{NWO: "actions/checkout", Ref: "v4", Tag: "v4", Branch: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HashAlgo: "sha1"},
	}
	require.NoError(t, store.Set(context.Background(), wfKey, seed, nil, nil))
	require.NoError(t, store.Save())

	// Re-pin: checkout bumps (Pinned), private repo 403s (Unresolved).
	rec := &Record{
		Entries: []Entry{
			{NWO: "actions/checkout", Ref: "v5", SHA: "cccccccccccccccccccccccccccccccccccccccc", Resolution: Pinned, Direct: true, OnBranch: "main", Workflows: []string{wfPath}},
			{NWO: "corp/private", Ref: "v2", Resolution: Unresolved, Issue: "sso-required", Reason: "403 SSO authorization required", Workflows: []string{wfPath}},
		},
	}
	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v5", Branch: "main", SHA: "cccccccccccccccccccccccccccccccccccccccc", HashAlgo: "sha1"},
	}
	directKeys := map[string]bool{"actions/checkout@v5": true}

	got := retainUnresolvablePins(rec, store, wfPath, deps, directKeys)

	require.Len(t, got, 2, "unresolved pin must be retained alongside the bumped pin")
	assert.True(t, directKeys["corp/private@v2"], "retained unresolved pin must stay direct")

	// Verify it survives a write round-trip.
	require.NoError(t, store.Set(context.Background(), wfKey, got, buildParentMap(rec, wfPath), directKeys))
	require.NoError(t, store.Save())

	after, err := store.Get(wfKey)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, d := range after {
		names[d.NWO] = true
	}
	assert.True(t, names["corp/private"], "unresolved pin must survive the re-pin write")
	assert.True(t, names["actions/checkout"], "bumped pin must be written")
}
