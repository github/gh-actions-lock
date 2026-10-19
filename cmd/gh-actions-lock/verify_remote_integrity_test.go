package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func isolateRemoteWrites(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "LOCALAPPDATA", "GH_CONFIG_DIR"} {
		t.Setenv(key, root)
	}
	t.Setenv("GH_HOST", "github.com")
	t.Setenv("GH_ACTIONS_LOCK_REPO_ROOT", filepath.Join(root, "must-not-be-used"))
	t.Setenv("GH_ACTIONS_LOCK_WORKFLOWS_DIR", filepath.Join(root, "must-not-be-used"))
	// No git or gh executable is available: the API path must not clone or
	// inspect the cwd repository, even when local overrides are set.
	t.Setenv("PATH", root)
	t.Chdir(root)
	t.Cleanup(func() { assertEmptyDir(t, root) })
	return root
}

func TestVerifyRemote_ReadOnlySnapshot(t *testing.T) {
	reg := &httpmock.Registry{}
	registerParity(reg)
	registerNodeManifests(reg)
	rr := newRemoteRepo(map[string]string{
		".github/workflows/ci.yml":       strings.Replace(remoteWorkflow, "actions/checkout@v6", "$/actions/build", 1),
		".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
		"actions/build/action.yml":       "runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
	}, reg)
	isolateRemoteWrites(t)
	commitLookups := 0
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.Path, "/repos/octo/api/commits/") {
			commitLookups++
			// A second resolution would observe a branch that moved.
			sha := remoteSHA
			if commitLookups > 1 {
				sha = graphLeafSHA
			}
			return httpmock.JSONResponse(map[string]any{"sha": sha})(req)
		}
		if strings.Contains(req.URL.Path, "/contents/") {
			require.Equal(t, remoteSHA, req.URL.Query().Get("ref"))
		}
		return rr.RoundTrip(req)
	})

	stdout, stderr, err := runCommandWithHTTP(t, rt, "--verify", "-R", "octo/api", "--json=valid,findings")
	require.NoError(t, err, "%s\n%s", stdout, stderr)
	assert.Equal(t, 1, commitLookups)
	assert.NotContains(t, stderr, "Run log:")
	var payload parityPayload
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	reg.Verify(t)
}

func TestVerifyRemote_RejectsNonCommitSnapshot(t *testing.T) {
	for _, sha := range []string{"", "main", "0123456", strings.Repeat("z", 40)} {
		t.Run(fmt.Sprintf("sha=%q", sha), func(t *testing.T) {
			rr := newRemoteRepo(nil, &httpmock.Registry{})
			isolateRemoteWrites(t)
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "/commits/") {
					return httpmock.JSONResponse(map[string]any{"sha": sha})(req)
				}
				require.NotContains(t, req.URL.Path, "/contents/")
				return rr.RoundTrip(req)
			})
			stdout, _, err := runCommandWithHTTP(t, rt, "--verify", "octo/api", "--json=valid")
			require.ErrorContains(t, err, "full commit SHA")
			assert.Empty(t, stdout)
		})
	}
}

func TestVerifyRemote_GraphIntegrity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*parserlock.File)
		fault  bool
		want   string
	}{
		{name: "complete graph"},
		{
			name: "dangling child",
			mutate: func(f *parserlock.File) {
				delete(f.Dependencies, "acme/child@v2")
			},
			want: "acme/child@v2",
		},
		{
			name: "missing workflow root",
			mutate: func(f *parserlock.File) {
				delete(f.Dependencies, "acme/parent@v1.0.0")
			},
			want: "acme/parent@v1.0.0",
		},
		{
			name: "erased edge with child retained",
			mutate: func(f *parserlock.File) {
				p := f.Dependencies["acme/parent@v1.0.0"]
				p.Uses = nil
				f.Dependencies["acme/parent@v1.0.0"] = p
			},
			want: "lockfile omits that edge",
		},
		{
			name: "erased edge with child removed",
			mutate: func(f *parserlock.File) {
				p := f.Dependencies["acme/parent@v1.0.0"]
				p.Uses = nil
				f.Dependencies["acme/parent@v1.0.0"] = p
				delete(f.Dependencies, "acme/child@v2")
			},
			want: "lockfile omits that edge",
		},
		{name: "unreadable locked child", fault: true, want: "could not verify locked dependency manifests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf, file := generateGraph(t)
			if tt.mutate != nil {
				tt.mutate(&file)
			}
			lock, err := yaml.Marshal(file)
			require.NoError(t, err)
			rr := newRemoteRepo(map[string]string{wf: graphWorkflow, parserlock.Path: string(lock)}, nil)
			rr.fallback = &graphTransport{failChild: tt.fault, movedParent: true}
			isolateRemoteWrites(t)
			p, err := runParity(t, rr, "--verify", "-R", "octo/api")
			if tt.want == "" {
				require.NoError(t, err)
				assert.True(t, p.Valid, "%+v", p.Findings)
				return
			}
			require.ErrorIs(t, err, errSilent)
			assert.False(t, p.Valid)
			findings, err := json.Marshal(p.Findings)
			require.NoError(t, err)
			assert.Contains(t, string(findings), tt.want)
		})
	}
}

func TestVerifyRemote_UnknownContentFailsClosed(t *testing.T) {
	stages := []struct {
		name string
		path string
	}{
		{name: "metadata", path: "/repos/octo/api"},
		{name: "snapshot", path: "/repos/octo/api/commits/main"},
		{name: "inventory", path: "/repos/octo/api/contents/.github/workflows"},
		{name: "workflow", path: "/repos/octo/api/contents/.github/workflows/ci.yml"},
		{name: "lockfile", path: "/repos/octo/api/contents/.github/workflows/actions.lock"},
		{name: "self action", path: "/repos/octo/api/contents/actions/build/action.yml"},
		{name: "locked manifests", path: "/graphql"},
	}
	failures := []struct {
		name   string
		status int
		err    error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "server failure", status: http.StatusServiceUnavailable},
		{name: "transport failure", err: errors.New("test transport failed")},
		{name: "cancelled", err: context.Canceled},
	}
	for _, stage := range stages {
		for _, failure := range failures {
			t.Run(stage.name+"/"+failure.name, func(t *testing.T) {
				reg := &httpmock.Registry{}
				registerParity(reg)
				registerNodeManifests(reg)
				rr := newRemoteRepo(map[string]string{
					".github/workflows/ci.yml":       remoteWorkflow,
					".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
				}, reg)
				if stage.name == "self action" {
					rr.files[".github/workflows/ci.yml"] = strings.Replace(remoteWorkflow, "actions/checkout@v6", "$/actions/build", 1)
					rr.files["actions/build/action.yaml"] = nodeActionYAML
				}
				isolateRemoteWrites(t)
				reached := false
				rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == stage.path {
						reached = true
						if failure.err != nil {
							return nil, failure.err
						}
						return httpmock.StatusResponse(failure.status)(req)
					}
					require.NotContains(t, req.URL.Path, "action.yaml", "non-404 errors must not try a different manifest")
					return rr.RoundTrip(req)
				})

				stdout, _, err := runCommandWithHTTP(t, rt, "--verify", "octo/api", "--json=valid,findings")
				require.Error(t, err)
				assert.True(t, reached)
				if stdout != "" {
					var p parityPayload
					require.NoError(t, json.Unmarshal([]byte(stdout), &p))
					assert.False(t, p.Valid)
				}
			})
		}
	}
}

func TestVerifyRemote_MalformedFiles(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		body string
	}{
		{name: "lockfile", path: parserlock.Path, body: "version: [\n"},
		{name: "workflow", path: ".github/workflows/ci.yml", body: "jobs: [\n"},
		{name: "empty lockfile", path: parserlock.Path, body: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			rr := newRemoteRepo(map[string]string{
				".github/workflows/ci.yml": remoteWorkflow,
				parserlock.Path:            remoteLock("actions/checkout@v6"),
			}, reg)
			rr.files[tt.path] = tt.body
			isolateRemoteWrites(t)
			stdout, _, err := runCommandWithHTTP(t, rr, "--verify", "octo/api", "--json=valid,findings")
			require.Error(t, err)
			if stdout != "" {
				var p parityPayload
				require.NoError(t, json.Unmarshal([]byte(stdout), &p))
				assert.False(t, p.Valid)
			}
		})
	}
}

func TestVerifyRemote_DiscoveryChecksMissingReusableWorkflow(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			rr := newRemoteRepo(map[string]string{
				".github/workflows/ci.yml": "jobs:\n  call:\n    uses: ./.github/workflows/missing.yml\n",
				parserlock.Path:            remoteLock(),
			}, &httpmock.Registry{})
			isolateRemoteWrites(t)
			args := []string{"--verify", "octo/api"}
			if explicit {
				args = append(args, ".github/workflows/ci.yml")
			}
			_, _, err := runCommandWithHTTP(t, rr, args...)
			require.ErrorContains(t, err, "missing.yml called by .github/workflows/ci.yml")
		})
	}
}

func TestVerifyRemote_RejectsProfilesWithoutWrites(t *testing.T) {
	root := isolateRemoteWrites(t)
	_, _, err := runCommandWithHTTP(t, &httpmock.Registry{}, "--verify", "octo/api", "--profile", filepath.Join(root, "profile"))
	require.ErrorContains(t, err, "never writes profiles")
	_, err = os.Stat(filepath.Join(root, "profile"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestVerifyRemote_ActualRedirectPreservesSnapshotAndWarns(t *testing.T) {
	reg := &httpmock.Registry{}
	registerParity(reg)
	registerNodeManifests(reg)
	rr := newRemoteRepo(map[string]string{
		".github/workflows/ci.yml": remoteWorkflow,
		parserlock.Path:            remoteLock("actions/checkout@v6"),
	}, reg)
	isolateRemoteWrites(t)
	metadataRedirects, contentRedirects := 0, 0
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/repos/octo/api":
			metadataRedirects++
			resp, err := httpmock.StatusResponse(http.StatusMovedPermanently)(req)
			resp.Header.Set("Location", "/repos/acme/new")
			return resp, err
		case req.URL.Path == "/repos/acme/new":
			return httpmock.JSONResponse(map[string]any{"full_name": "acme/new", "default_branch": "main"})(req)
		case strings.HasPrefix(req.URL.Path, "/repos/octo/api/contents/"):
			contentRedirects++
			resp, err := httpmock.StatusResponse(http.StatusMovedPermanently)(req)
			resp.Header.Set("Location", strings.Replace(req.URL.Path, "/octo/api/", "/acme/new/", 1)+"?"+req.URL.RawQuery)
			return resp, err
		case strings.HasPrefix(req.URL.Path, "/repos/acme/new/contents/"):
			require.Equal(t, remoteSHA, req.URL.Query().Get("ref"))
			clone := req.Clone(req.Context())
			clone.URL.Path = strings.Replace(clone.URL.Path, "/acme/new/", "/octo/api/", 1)
			return rr.RoundTrip(clone)
		}
		return rr.RoundTrip(req)
	})

	p, err := runParity(t, rt, "--verify", "octo/api")
	require.NoError(t, err)
	assert.True(t, p.Valid)
	assert.Equal(t, 1, metadataRedirects)
	assert.Greater(t, contentRedirects, 0)
	require.Len(t, p.Findings, 1)
	assert.Equal(t, "repo-renamed", p.Findings[0].Category)
	assert.Contains(t, p.Findings[0].Detail, "acme/new")
	reg.Verify(t)
}

func TestVerifyRemote_RejectsCrossHostRedirect(t *testing.T) {
	isolateRemoteWrites(t)
	calls := 0
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "api.github.com", req.URL.Host)
		resp, err := httpmock.StatusResponse(http.StatusMovedPermanently)(req)
		resp.Header.Set("Location", "https://other.example/repos/octo/api")
		return resp, err
	})
	stdout, _, err := runCommandWithHTTP(t, rt, "--verify", "octo/api", "--json=valid")
	require.ErrorContains(t, err, "refusing API redirect")
	assert.Equal(t, 1, calls)
	assert.Empty(t, stdout)
}
