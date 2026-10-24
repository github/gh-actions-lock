package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/refresh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	updateOldSHA = "1111111111111111111111111111111111111111"
	updateNewSHA = "2222222222222222222222222222222222222222"
)

// writeMovedWorkflows writes workflows (name → uses) and a lockfile that
// locks every action at updateOldSHA, while upstream every ref has moved
// ahead to updateNewSHA.
func writeMovedWorkflows(t *testing.T, reg *httpmock.Registry, workflows map[string][]string) {
	t.Helper()
	commits, heads := map[string]string{}, map[string]string{}
	dir := t.TempDir()
	wfDir := filepath.Join(dir, ".github", "workflows")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(wfDir, 0o755))
	deps := map[string]bool{}
	var lockWorkflows strings.Builder
	for name, uses := range workflows {
		body := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n"
		lockWorkflows.WriteString("  '.github/workflows/" + name + "':\n")
		for _, u := range uses {
			body += "      - uses: " + u + "\n"
			lockWorkflows.WriteString("    - '" + u + "'\n")
			deps[u] = true
			nwo, _, _ := strings.Cut(u, "@")
			commits[nwo], heads[nwo] = updateNewSHA, updateNewSHA
		}
		require.NoError(t, os.WriteFile(filepath.Join(wfDir, name), []byte(body), 0o600))
	}
	lock := "version: 'v0.0.3'\ndependencies:\n"
	for u := range deps {
		ref := u[strings.Index(u, "@")+1:]
		lock += "  '" + u + "':\n    ref: '" + ref + "'\n    commit: 'sha1-" + updateOldSHA + "'\n    owner_id: 1\n    repo_id: 1\n"
	}
	lock += "workflows:\n" + lockWorkflows.String()
	require.NoError(t, os.WriteFile(filepath.Join(wfDir, "actions.lock"), []byte(lock), 0o600))
	stubGraph(t, reg, heads, commits, nil)
	reg.RegisterReusable(httpmock.REST("GET", `/compare/`), httpmock.JSONResponse(map[string]any{
		"status": "ahead", "merge_base_commit": map[string]any{"sha": updateOldSHA},
	}))
	reg.RegisterReusable(httpmock.REST("GET", `^/repos/[^/]+/[^/]+$`), httpmock.JSONResponse(map[string]any{"id": 1, "owner": map[string]any{"id": 1}}))
	useDefaultTransport(t, reg)
	t.Chdir(dir)
}

// lockedCommit returns the commit the lockfile records for key.
func lockedCommit(t *testing.T, key string) string {
	t.Helper()
	lock := readTempLockfilePins(t)
	_, after, ok := strings.Cut(lock, "'"+key+"':")
	require.True(t, ok, "no entry %s in\n%s", key, lock)
	_, after, ok = strings.Cut(after, "commit: 'sha1-")
	require.True(t, ok)
	return after[:40]
}

func TestUpdate_SelectorMovesOnlyThatEntry(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1", "octo/b@v1"}})

	_, stderr, err := runCommandWithHTTP(t, reg, "update", "octo/a@v1", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@v1"), "the selected entry moves")
	assert.Equal(t, updateOldSHA, lockedCommit(t, "octo/b@v1"), "an unselected moved ref stays locked")
}

func TestUpdate_ActionSelectorMatchesEveryRef(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1", "octo/b@v1"}, "other.yml": {"Octo/A@main"}})

	_, stderr, err := runCommandWithHTTP(t, reg, "update", "octo/a", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@v1"))
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@main"), "owner/repo matches case-insensitively")
	assert.Equal(t, updateOldSHA, lockedCommit(t, "octo/b@v1"))
}

func TestUpdate_NoArgsMovesEverything(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1", "octo/b@v1"}})

	_, stderr, err := runCommandWithHTTP(t, reg, "update", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@v1"))
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/b@v1"))
}

func TestUpdate_ScopedRunNamesUnscannedSharers(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{
		"ci.yml":    {"octo/a@v1"},
		"other.yml": {"octo/a@v1", "octo/b@v1"},
	})

	_, stderr, err := runCommandWithHTTP(t, reg, "update", ".github/workflows/ci.yml", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@v1"))
	assert.Equal(t, updateOldSHA, lockedCommit(t, "octo/b@v1"), "other.yml's own entries stay")
	assert.Contains(t, stderr, "Moved 1 shared entry that workflows outside this run also use")
	assert.Contains(t, stderr, "octo/a@v1 (2222222) also moved for .github/workflows/other.yml")
}

func TestUpdate_FullScanReportsNoUnscannedSharers(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1"}, "other.yml": {"octo/a@v1"}})

	_, stderr, err := runCommandWithHTTP(t, reg, "update", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.NotContains(t, stderr, "also moved for")
}

func TestUpdate_RelockIsADeprecatedAlias(t *testing.T) {
	reg := &httpmock.Registry{}
	writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1"}})

	_, stderr, err := runCommandWithHTTP(t, reg, "--relock", "--no-narrow", "--no-interactive")
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "--relock is deprecated; use `gh actions-lock update`")
	assert.Equal(t, updateNewSHA, lockedCommit(t, "octo/a@v1"))
}

func TestUpdate_RejectsUnusableArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"selector matching no entry", []string{"update", "nope/nope"}, "no lockfile entry matches nope/nope"},
		{"missing workflow file", []string{"update", ".github/workflows/missing.yml"}, `".github/workflows/missing.yml" is neither a workflow file nor an owner/repo[@ref] action`},
		{"verify", []string{"update", "--verify"}, "cannot be combined with --verify"},
		{"accept-moved", []string{"update", "--accept-moved"}, "--accept-moved is a separate step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			writeMovedWorkflows(t, reg, map[string][]string{"ci.yml": {"octo/a@v1"}})
			before := snapshotWorkflows(t)

			_, _, err := runCommandWithHTTP(t, reg, tt.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Equal(t, before, snapshotWorkflows(t), "nothing is written")
		})
	}
}

func TestSplitUpdateArgs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ci.yml"), nil, 0o600))
	t.Chdir(dir)

	paths, sels, err := splitUpdateArgs([]string{"ci.yml", "octo/a@v1", "octo/b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"ci.yml"}, paths)
	assert.Equal(t, []refresh.Selector{{NWO: "octo/a", Ref: "v1"}, {NWO: "octo/b"}}, sels)

	_, _, err = splitUpdateArgs([]string{"."})
	assert.Error(t, err, "a directory is not a workflow file")
}
