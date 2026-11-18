package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type requestCountingTransport struct {
	calls atomic.Int64
	// parity answers parity queries without counting them.
	parity bool
	// tags, when set, answers tag and release listings and exact ref lookups
	// (tag name to commit) without
	// counting them: a SHA repair must prove its tag peels to the commit.
	tags map[string]string
}

func (t *requestCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.parity && httpmock.GraphQL(`commit: object\(oid`)(req) {
		return parityOK(req)
	}
	if t.parity && lockedManifestRead(req) {
		return actionFiles(nil, nil)(req)
	}
	if t.tags != nil && strings.HasSuffix(req.URL.Path, "/releases") {
		return httpmock.JSONResponse([]any{})(req)
	}
	if _, name, ok := strings.Cut(req.URL.Path, "/git/ref/"); t.tags != nil && ok {
		kind, name, _ := strings.Cut(name, "/")
		if sha, found := t.tags[name]; found && kind == "tags" {
			return httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/"+name, sha))(req)
		}
		return httpmock.StatusResponse(http.StatusNotFound)(req)
	}
	if t.tags != nil && strings.HasSuffix(req.URL.Path, "/tags") {
		var list []any
		for name, sha := range t.tags {
			list = append(list, map[string]any{"name": name, "commit": map[string]any{"sha": sha}})
		}
		return httpmock.JSONResponse(list)(req)
	}
	t.calls.Add(1)
	return nil, errors.New("unexpected HTTP request")
}

func TestInvalidSelfRepositoryRefDoesNotPinOtherRefs(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/actions/bad@v1
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)
	require.ErrorIs(t, err, errSilent)
	assert.Zero(t, transport.calls.Load(), "invalid syntax should be rejected before dependency resolution")

	gotWorkflow, err := os.ReadFile(workflowPath)
	require.NoError(t, err)
	assert.Equal(t, workflow, gotWorkflow)

	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	if lockContent, readErr := os.ReadFile(lockPath); readErr == nil {
		assert.NotContains(t, string(lockContent), "actions/checkout")
	} else {
		assert.ErrorIs(t, readErr, os.ErrNotExist)
	}
}

func TestInvalidSelfRepositoryRefPreventsNetwork(t *testing.T) {
	transport := &requestCountingTransport{}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	localActionPath := filepath.Join(dir, "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(localActionPath), 0o755))
	require.NoError(t, os.WriteFile(localActionPath, []byte("runs:\n  using: composite\n"), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: ./actions/local
      - uses: "$/actions/bad@${{ matrix.ref }}"
      - uses: actions/checkout@v4
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	t.Chdir(dir)

	_, _, err := runCommandWithHTTP(t, transport, workflowPath)

	require.Error(t, err)
	assert.Zero(t, transport.calls.Load(), "invalid syntax should stop the run before dependency resolution")
	gotWorkflow, readErr := os.ReadFile(workflowPath)
	require.NoError(t, readErr)
	assert.Equal(t, workflow, gotWorkflow)
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	_, readErr = os.Stat(lockPath)
	assert.ErrorIs(t, readErr, os.ErrNotExist)
}

// TestExistingSHARefKeepsSelfRepositoryActionPin: a `$/…` action's SHA pin
// stays as written and locked under ci.yml, even though other.yml locks the
// same commit by tag (#264).
func TestExistingSHARefKeepsSelfRepositoryActionPin(t *testing.T) {
	const sha = "bcd2ba49218906704ab6c1aa796996da409d3eb1"
	transport := &requestCountingTransport{parity: true, tags: map[string]string{"v3.2.0": sha}}

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	actionPath := filepath.Join(dir, ".github", "actions", "local", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(actionPath), 0o755))
	require.NoError(t, os.WriteFile(actionPath, []byte(`name: Local
runs:
  using: composite
  steps:
    - uses: actions/create-github-app-token@`+sha+`
`), 0o600))

	workflowPath := filepath.Join(dir, ".github", "workflows", "ci.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(workflowPath), 0o755))
	workflow := []byte(`name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: $/.github/actions/local
`)
	require.NoError(t, os.WriteFile(workflowPath, workflow, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(workflowPath), "other.yml"), []byte(`name: Other
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/create-github-app-token@v3.2.0
`), 0o600))
	lockPath := filepath.Join(dir, ".github", "workflows", "actions.lock")
	originalLock := []byte(`version: 'v0.0.2'
workflows:
    '.github/workflows/ci.yml':
        - 'actions/create-github-app-token@` + sha + `'
    '.github/workflows/other.yml':
        - 'actions/create-github-app-token@v3.2.0'
dependencies:
    'actions/create-github-app-token@` + sha + `':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/checkout@v4'
    'actions/create-github-app-token@v3.2.0':
        ref: 'v3.2.0'
        commit: 'sha1-` + sha + `'
        owner_id: 44036562
        repo_id: 642580244
        uses:
            - 'actions/setup-go@v5'
    'actions/checkout@v4':
        ref: 'v4'
        commit: 'sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
        owner_id: 44036562
        repo_id: 197814629
    'actions/setup-go@v5':
        ref: 'v5'
        commit: 'sha1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
        owner_id: 44036562
        repo_id: 485264523
`)
	require.NoError(t, os.WriteFile(lockPath, originalLock, 0o600))
	t.Chdir(dir)

	originalAction, err := os.ReadFile(actionPath)
	require.NoError(t, err)

	for _, args := range [][]string{{filepath.Join(".github", "workflows", "ci.yml")}, nil} {
		_, _, err = runCommandWithHTTP(t, transport, args...)
		require.NoError(t, err)
		assert.Zero(t, transport.calls.Load())

		action, err := os.ReadFile(actionPath)
		require.NoError(t, err)
		assert.Equal(t, originalAction, action)

		lock, err := os.ReadFile(lockPath)
		require.NoError(t, err)
		assert.Contains(t, string(lock), "    '.github/workflows/ci.yml':\n        - 'actions/create-github-app-token@"+sha+"'\n")
		assert.Contains(t, string(lock), "    'actions/create-github-app-token@"+sha+"':\n        ref: 'v3.2.0'\n")
		assert.Contains(t, string(lock), "uses:\n            - 'actions/checkout@v4'")
	}
}

// TestSelfRepositoryActionSHAPinKeepsRenamedName: a `$/…` action pins a
// renamed repository by SHA. The action file is left alone and the lockfile
// covers the pin under the name it's written with.
func TestSelfRepositoryActionSHAPinKeepsRenamedName(t *testing.T) {
	const (
		oldName = "krzema12/github-actions-typing"
		newName = "typesafegithub/github-actions-typing"
	)
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
	reg.RegisterReusable(httpmock.GraphQLForRepo("krzema12", "github-actions-typing"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": withRepoID(testRepoResponse(newName, parityTestSHA, nodeActionYAML), 1)},
	}))
	for _, nwo := range []string{oldName, newName} {
		reg.RegisterReusable(httpmock.REST("GET", `^/repos/`+nwo+`$`), httpmock.JSONResponse(map[string]any{
			"full_name": newName, "id": 1, "default_branch": "main", "owner": map[string]any{"id": 42},
		}))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", parityTestSHA)))
	}
	httpmock.NoTagsNamed(reg, "typesafegithub", "github-actions-typing")
	isolateRunLogs(t)
	path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/.github/actions/typing\n")
	require.NoError(t, os.Mkdir(".git", 0o755))
	action := filepath.Join(".github", "actions", "typing", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(action), 0o755))
	require.NoError(t, os.WriteFile(action, []byte(composite(oldName+"@"+parityTestSHA)), 0o600))

	_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive")
	require.NoError(t, err, stderr)
	got, err := os.ReadFile(action)
	require.NoError(t, err)
	assert.Equal(t, composite(oldName+"@"+parityTestSHA), string(got))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "    '"+path+"':\n        - '"+oldName+"@"+parityTestSHA+"'\n")
	_, stderr, err = runCommandWithHTTP(t, parityFallback{reg}, "--verify-local")
	require.NoError(t, err, "%s\n%s", stderr, lock)
	_, stderr, err = runCommandWithHTTP(t, parityFallback{reg}, "--verify")
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "change it to `uses: "+newName+"@"+parityTestSHA+"` in the same-repository action that uses it")
}

// TestSelfRepositoryActionSHAPinKeepsRenamedNameAcrossSpellings: a workflow
// uses a renamed repository's new name at the commit a `$/…` action pins
// under the old name. The action file is left alone and the lockfile covers
// both spellings.
func TestSelfRepositoryActionSHAPinKeepsRenamedNameAcrossSpellings(t *testing.T) {
	const (
		oldName = "krzema12/github-actions-typing"
		newName = "typesafegithub/github-actions-typing"
	)
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), renamedParity)
	for _, owner := range []string{"krzema12", "typesafegithub"} {
		reg.RegisterReusable(httpmock.GraphQLForRepo(owner, "github-actions-typing"), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": withRepoID(testRepoResponse(newName, parityTestSHA, nodeActionYAML), 1)},
		}))
	}
	for _, nwo := range []string{oldName, newName} {
		reg.RegisterReusable(httpmock.REST("GET", `^/repos/`+nwo+`$`), httpmock.JSONResponse(map[string]any{
			"full_name": newName, "id": 1, "default_branch": "main", "owner": map[string]any{"id": 42},
		}))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/tags`), httpmock.JSONResponse(httpmock.TagListResponse("v2.2.2", parityTestSHA)))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/releases`), httpmock.JSONResponse([]map[string]any{}))
		reg.RegisterReusable(httpmock.REST("GET", `repos/`+nwo+`/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", parityTestSHA)))
		reg.RegisterReusable(httpmock.REST("GET", `^/repos/`+nwo+`/git/ref/tags/v2\.2\.2$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.2.2", parityTestSHA)))
		reg.RegisterReusable(httpmock.REST("GET", `^/repos/`+nwo+`/git/ref/heads/main$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/heads/main", parityTestSHA)))
	}
	httpmock.NoTagsNamed(reg, "krzema12", "github-actions-typing")
	httpmock.NoTagsNamed(reg, "typesafegithub", "github-actions-typing")
	isolateRunLogs(t)
	path := writeTempWorkflow(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/.github/actions/typing\n      - uses: "+newName+"@"+parityTestSHA+"\n")
	require.NoError(t, os.Mkdir(".git", 0o755))
	action := filepath.Join(".github", "actions", "typing", "action.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(action), 0o755))
	require.NoError(t, os.WriteFile(action, []byte(composite(oldName+"@"+parityTestSHA)), 0o600))

	_, stderr, err := runCommandWithHTTP(t, parityFallback{reg}, "--no-interactive")
	require.NoError(t, err, stderr)
	got, err := os.ReadFile(action)
	require.NoError(t, err)
	assert.Equal(t, composite(oldName+"@"+parityTestSHA), string(got))
	lock := readTempLockfilePins(t)
	assert.Contains(t, lock, "    '"+path+"':\n        - '"+oldName+"@"+parityTestSHA+"'\n")
	assert.Contains(t, lock, "        - '"+newName+"@"+parityTestSHA+"'\n")
	_, stderr, err = runCommandWithHTTP(t, parityFallback{reg}, "--verify-local")
	require.NoError(t, err, "%s\n%s", stderr, lock)
}
