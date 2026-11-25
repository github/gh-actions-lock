package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	remoteRepoAPI = "/repos/octo/project"
	remoteSHA     = "0123456789abcdef0123456789abcdef01234567"
	remoteLock    = ".github/workflows/actions.lock"
	remoteCI      = ".github/workflows/ci.yml"
)

func TestParseRemoteTarget(t *testing.T) {
	tests := []struct {
		name                string
		input, host, ref    string
		wantHost, wantRef   string
		wantFile, wantError string
	}{
		{name: "owner and repo", input: "octo/project", wantHost: "github.com"},
		{name: "repository URL", input: "https://github.com/octo/project.git/", wantHost: "github.com"},
		{name: "workflow directory", input: "https://github.com/octo/project/.github/workflows", wantHost: "github.com"},
		{name: "tree URL with slash in branch", input: "https://tenant.ghe.com/octo/project/tree/Release/v1/.github/workflows/", wantHost: "tenant.ghe.com", wantRef: "Release/v1"},
		{name: "workflow URL with encoded branch", input: "https://github.com/octo/project/blob/Feature%2FCI/.github/workflows/ci.yml?plain=1#L3", wantHost: "github.com", wantRef: "Feature/CI", wantFile: remoteCI},
		{name: "host and ref overrides", input: "octo/project", host: "Tenant.ghe.com", ref: "refs/tags/v1", wantHost: "tenant.ghe.com", wantRef: "refs/tags/v1"},
		{name: "unsupported host", input: "https://github.com.example.com/octo/project", wantError: "unsupported GitHub host"},
		{name: "conflicting host", input: "https://github.com/octo/project", host: "tenant.ghe.com", wantError: "does not match"},
		{name: "conflicting ref", input: "https://github.com/octo/project/tree/main/.github/workflows", ref: "other", wantError: "does not match"},
		{name: "not a workflow file", input: "https://github.com/octo/project/blob/main/action.yml", wantError: "expected a workflow file"},
		{name: "revision expression", input: "octo/project", ref: "main~1", wantError: "invalid Git ref"},
	}
	t.Setenv("GH_HOST", "")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRemoteTarget(tt.input, tt.host, tt.ref)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "octo/project", got.nwo())
			assert.Equal(t, tt.wantHost, got.host)
			assert.Equal(t, tt.wantRef, got.ref)
			assert.Equal(t, tt.wantFile, got.workflow)
		})
	}
}

func remoteBranch(reg *httpmock.Registry, branch, sha string) {
	reg.Register(httpmock.REST("GET", remoteRepoAPI+`/git/ref/heads/`+regexp.QuoteMeta(branch)+`$`),
		httpmock.JSONResponse(map[string]any{
			"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha},
		}))
}

func remoteFile(reg *httpmock.Registry, path, body string) {
	reg.Register(httpmock.RESTWithQuery("GET", remoteRepoAPI+`/contents/`+regexp.QuoteMeta(path)+`$`, "ref="+remoteSHA),
		httpmock.JSONResponse(map[string]string{
			"path": path, "type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(body)),
		}))
}

func remoteMissing(reg *httpmock.Registry, path string) {
	reg.Register(httpmock.RESTWithQuery("GET", remoteRepoAPI+`/contents/`+regexp.QuoteMeta(path)+`$`, "ref="+remoteSHA),
		httpmock.StatusResponse(http.StatusNotFound))
}

func TestRemoteCheckUsesSelectedWorkflowAndItsComposite(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", "/not/the/remote/root")
	reg := &httpmock.Registry{}
	remoteBranch(reg, "Feature/ci", remoteSHA)
	remoteFile(reg, remoteCI, "jobs:\n  ci:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/tools/test\n")
	remoteFile(reg, "tools/test/action.yml", "runs:\n  using: composite\n  steps:\n    - run: echo ok\n      shell: bash\n")
	remoteMissing(reg, remoteLock)

	stdout, stderr, err := runCommandWithHTTP(t, reg, "--json", "https://github.com/octo/project/blob/Feature/ci/"+remoteCI)
	require.ErrorIs(t, err, errSilent, "%s\n%s", stdout, stderr)
	assert.Contains(t, stdout, remoteCI)
	assert.Contains(t, stdout, "workflow has no lockfile entry")
	assert.NotContains(t, stdout, "can't inspect self repository actions")
	_, err = os.Stat(".github")
	assert.True(t, os.IsNotExist(err))
	reg.Verify(t)
}

func TestRemotePublishesNewLockfile(t *testing.T) {
	for _, mode := range []string{"commit", "pr", "pr-base-moved"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("GH_HOST", "")
			reg := &httpmock.Registry{}
			reg.Register(httpmock.REST("GET", "^"+remoteRepoAPI+"$"), httpmock.JSONResponse(map[string]string{"default_branch": "main"}))
			remoteBranch(reg, "main", remoteSHA)
			reg.Register(httpmock.RESTWithQuery("GET", remoteRepoAPI+`/contents/\.github/workflows$`, "ref="+remoteSHA),
				httpmock.JSONResponse([]map[string]string{{"name": "ci.yml", "type": "file"}, {"name": "other", "type": "dir"}}))
			remoteMissing(reg, remoteLock)
			remoteMissing(reg, ".github/dependabot.yml")
			remoteMissing(reg, ".github/dependabot.yaml")
			remoteFile(reg, remoteCI, "jobs:\n  ci:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-go@v5\n")
			registerSetupGo(reg)

			head, flag := "main", "--commit"
			const commitURL = "https://github.com/octo/project/commit/1"
			const prURL = "https://github.com/octo/project/pull/42"
			switch mode {
			case "pr-base-moved":
				flag = "--pr"
				remoteBranch(reg, "main", strings.Repeat("a", 40))
			case "pr":
				flag = "--pr"
				remoteBranch(reg, "main", remoteSHA)
				reg.Register(httpmock.REST("POST", remoteRepoAPI+`/git/refs$`), func(req *http.Request) (*http.Response, error) {
					var data map[string]string
					if err := json.NewDecoder(req.Body).Decode(&data); err != nil {
						return nil, err
					}

					assert.Contains(t, data["ref"], "refs/heads/actions-lock/")
					assert.Equal(t, remoteSHA, data["sha"])
					head = strings.TrimPrefix(data["ref"], "refs/heads/")
					return httpmock.JSONResponse(data)(req)
				})
				reg.Register(httpmock.REST("POST", remoteRepoAPI+`/pulls$`), func(req *http.Request) (*http.Response, error) {
					var data map[string]string
					if err := json.NewDecoder(req.Body).Decode(&data); err != nil {
						return nil, err
					}
					assert.Equal(t, head, data["head"])
					assert.Equal(t, "main", data["base"])
					return httpmock.JSONResponse(map[string]string{"html_url": prURL})(req)
				})
			}
			if mode != "pr-base-moved" {
				reg.Register(httpmock.GraphQL(`createCommitOnBranch`), httpmock.GraphQLQuery(
					`{"data":{"createCommitOnBranch":{"commit":{"url":"`+commitURL+`"}}}}`, func(_ string, vars map[string]any) {
						input := vars["input"].(map[string]any)
						assert.Equal(t, remoteSHA, input["expectedHeadOid"])
						assert.Equal(t, map[string]any{"repositoryNameWithOwner": "octo/project", "branchName": head}, input["branch"])
						changes := input["fileChanges"].(map[string]any)
						additions := changes["additions"].([]any)
						files := map[string]string{}
						for _, addition := range additions {
							file := addition.(map[string]any)
							body, err := base64.StdEncoding.DecodeString(file["contents"].(string))
							assert.NoError(t, err)
							files[file["path"].(string)] = string(body)
						}
						assert.Len(t, files, 2)
						assert.Contains(t, files[remoteLock], "actions/setup-go@v5.0.0")
						assert.Contains(t, files[remoteCI], "uses: actions/setup-go@v5.0.0")
					}))
			}

			stdout, stderr, err := runCommandWithHTTP(t, reg, "-R", "octo/project", flag, "--json")
			switch mode {
			case "pr-base-moved":
				require.ErrorContains(t, err, "moved since the scan")
			case "commit":
				require.NoError(t, err, stderr)
				assert.Contains(t, stderr, commitURL)
				assert.True(t, json.Valid([]byte(stdout)))
			case "pr":
				require.NoError(t, err, stderr)
				assert.Contains(t, stderr, prURL)
				assert.True(t, json.Valid([]byte(stdout)))
			}
			reg.Verify(t)
		})
	}
}

func TestRemotePRWithoutChangesDoesNotCreateBranch(t *testing.T) {
	t.Chdir(t.TempDir())
	reg := &httpmock.Registry{}
	remoteBranch(reg, "main", remoteSHA)
	remoteFile(reg, remoteCI, "jobs:\n  ci:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n")
	store, err := lockfile.LoadContents([]byte("version: "+parserlock.Version+"\nworkflows:\n  "+remoteCI+": []\n"), nil)
	require.NoError(t, err)
	body, err := store.Contents()
	require.NoError(t, err)
	remoteFile(reg, remoteLock, string(body))
	remoteMissing(reg, ".github/dependabot.yml")
	remoteMissing(reg, ".github/dependabot.yaml")

	_, stderr, err := runCommandWithHTTP(t, reg, "--pr", "https://github.com/octo/project/blob/main/"+remoteCI)
	require.NoError(t, err, stderr)
	assert.Contains(t, stderr, "No remote changes to commit")
	reg.Verify(t)
}

func TestRemoteCommitPrunesLockfileAfterLastWorkflowDeleted(t *testing.T) {
	t.Chdir(t.TempDir())
	reg := &httpmock.Registry{}
	remoteBranch(reg, "main", remoteSHA)
	remoteMissing(reg, workflowsPath)
	remoteFile(reg, remoteLock, "version: "+parserlock.Version+"\nworkflows:\n  "+remoteCI+": []\n")
	remoteMissing(reg, ".github/dependabot.yml")
	remoteMissing(reg, ".github/dependabot.yaml")
	reg.Register(httpmock.GraphQL(`createCommitOnBranch`), httpmock.GraphQLQuery(
		`{"data":{"createCommitOnBranch":{"commit":{"url":"https://github.com/octo/project/commit/1"}}}}`, func(_ string, vars map[string]any) {
			changes := vars["input"].(map[string]any)["fileChanges"].(map[string]any)
			assert.Equal(t, []any{map[string]any{"path": remoteLock}}, changes["deletions"])
			assert.NotContains(t, changes, "additions")
		}))
	_, stderr, err := runCommandWithHTTP(t, reg, "--commit", "https://github.com/octo/project/tree/main/"+workflowsPath)
	require.NoError(t, err, stderr)
	reg.Verify(t)
}
