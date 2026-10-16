package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/repospec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	remoteSHA      = "0123456789abcdef0123456789abcdef01234567"
	remoteCheckout = "de0fac2e4500dabe0009e67214ff5f5447ce83dd"
	remoteWorkflow = `
name: ci
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
`
)

func remoteLock(pins ...string) string {
	var deps, wf strings.Builder
	for _, key := range pins {
		deps.WriteString("  '" + key + "':\n    ref: 'v6'\n    commit: 'sha1-" + remoteCheckout + "'\n    owner_id: 1\n    repo_id: 1\n")
		wf.WriteString("    - '" + key + "'\n")
	}
	return "version: '" + parserlock.Version + "'\ndependencies:\n" + deps.String() +
		"workflows:\n  '.github/workflows/ci.yml':\n" + wf.String()
}

// remoteRepo serves a fake octo/api repository over the contents API and
// records the refs those reads were pinned to.
type remoteRepo struct {
	files    map[string]string
	fullName string
	fallback http.RoundTripper
	mu       sync.Mutex
	refs     []string
}

func (rr *remoteRepo) respond(req *http.Request) (*http.Response, error) {
	rr.mu.Lock()
	rr.refs = append(rr.refs, req.URL.Query().Get("ref"))
	rr.mu.Unlock()

	p := strings.TrimPrefix(req.URL.Path, "/repos/octo/api/contents/")
	if body, ok := rr.files[p]; ok {
		return httpmock.JSONResponse(map[string]any{
			"type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(body)),
		})(req)
	}
	var entries []map[string]any
	for name := range rr.files {
		if dir, base := filepath.ToSlash(filepath.Dir(name)), filepath.Base(name); dir == p {
			entries = append(entries, map[string]any{"name": base, "path": name, "type": "file"})
		}
	}
	if len(entries) > 0 {
		return httpmock.JSONResponse(entries)(req)
	}
	return httpmock.StatusResponse(http.StatusNotFound)(req)
}

// newRemoteRepo returns a transport serving octo/api (default branch main)
// repeatedly; httpmock stubs are single-use. Other requests go to reg.
func newRemoteRepo(files map[string]string, reg *httpmock.Registry) *remoteRepo {
	return &remoteRepo{files: files, fallback: reg}
}

func (rr *remoteRepo) RoundTrip(req *http.Request) (*http.Response, error) {
	p := req.URL.Path
	switch {
	case p == "/repos/octo/api":
		full := rr.fullName
		if full == "" {
			full = "octo/api"
		}
		return httpmock.JSONResponse(map[string]any{"full_name": full, "default_branch": "main", "visibility": "public"})(req)
	case strings.HasPrefix(p, "/repos/octo/api/commits/"):
		return httpmock.JSONResponse(map[string]any{"sha": remoteSHA})(req)
	case strings.HasPrefix(p, "/repos/octo/api/contents/"):
		return rr.respond(req)
	}
	return rr.fallback.RoundTrip(req)
}

func chdirEmpty(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "remote verify must not write to the cwd")
}

func TestVerifyRemote_DefaultBranchPasses(t *testing.T) {
	reg := &httpmock.Registry{}
	files := map[string]string{
		".github/workflows/ci.yml":      remoteWorkflow,
		".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
	}
	rr := newRemoteRepo(files, reg)
	registerParity(reg)
	cwd := chdirEmpty(t)

	stdout, _, err := runCommandWithHTTP(t, rr, "--verify", "--json=valid,findings", "octo/api")

	require.NoError(t, err)
	var payload struct {
		Valid bool `json:"valid"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.True(t, payload.Valid)
	require.NotEmpty(t, rr.refs)
	for _, ref := range rr.refs {
		assert.Equal(t, remoteSHA, ref, "every read is pinned to the resolved commit")
	}
	assertEmptyDir(t, cwd)
}

func TestVerifyRemote_ExplicitRefSkipsDefaultBranchLookup(t *testing.T) {
	reg := &httpmock.Registry{}
	files := map[string]string{
		".github/workflows/ci.yml":      remoteWorkflow,
		".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
	}
	rr := newRemoteRepo(files, reg)
	registerParity(reg)
	chdirEmpty(t)

	var asked []string
	var mu sync.Mutex
	spy := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, req.URL.Path)
		mu.Unlock()
		return rr.RoundTrip(req)
	})

	_, _, err := runCommandWithHTTP(t, spy, "--verify", "-R", "octo/api", "--ref", "release/1")

	require.NoError(t, err)
	assert.Contains(t, asked, "/repos/octo/api/commits/release/1")
}

func TestVerifyRemote_RenamedTarget(t *testing.T) {
	tests := []struct {
		name     string
		fullName string
		want     bool
	}{
		{name: "renamed", fullName: "octo/api-v2", want: true},
		{name: "transferred", fullName: "acme/api", want: true},
		{name: "case-only difference", fullName: "Octo/API"},
		{name: "unchanged", fullName: "octo/api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			rr := newRemoteRepo(map[string]string{
				".github/workflows/ci.yml":       remoteWorkflow,
				".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
			}, reg)
			rr.fullName = tt.fullName
			registerParity(reg)
			chdirEmpty(t)

			stdout, _, err := runCommandWithHTTP(t, rr, 			"--verify", "--json=valid,findings", "octo/api")

			require.NoError(t, err, "a rename never blocks")
			var payload struct {
				Valid    bool `json:"valid"`
				Findings []struct {
					Category string `json:"category"`
					Detail   string `json:"detail"`
				} `json:"findings"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
			assert.True(t, payload.Valid)
			var renamed []string
			for _, f := range payload.Findings {
				if f.Category == "repo-renamed" {
					renamed = append(renamed, f.Detail)
				}
			}
			if !tt.want {
				assert.Empty(t, renamed)
				return
			}
			require.Len(t, renamed, 1)
			assert.Contains(t, renamed[0], tt.fullName)
		})
	}
}

func TestRenamedTargetFinding(t *testing.T) {
	spec := &repospec.Spec{Owner: "octo", Repo: "api"}
	tests := []struct {
		name      string
		canonical string
		want      bool
	}{
		{name: "empty canonical name", canonical: ""},
		{name: "same name", canonical: "octo/api"},
		{name: "case-only", canonical: "OCTO/Api"},
		{name: "renamed", canonical: "octo/new", want: true},
		{name: "transferred", canonical: "acme/api", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := renamedTargetFinding(spec, tt.canonical)
			assert.Equal(t, tt.want, ok)
			if !ok {
				return
			}
			assert.Equal(t, checks.RepoRenamed, f.Category)
			assert.Equal(t, checks.SeverityWarning, f.Severity)
			assert.True(t, f.IsValid())
			assert.Contains(t, f.Detail, tt.canonical)
		})
	}
}

func TestVerifyRemote_MissingLockfileBlocks(t *testing.T) {
	reg := &httpmock.Registry{}
	rr := newRemoteRepo(map[string]string{".github/workflows/ci.yml": remoteWorkflow}, reg)
	reg.Register(
		httpmock.GraphQLForRepo("actions", "checkout"),
		httpmock.JSONResponse(map[string]any{"data": map[string]any{
			"a0": testRepoResponse("actions/checkout", remoteCheckout, nodeActionYAML),
		}}),
	)
	cwd := chdirEmpty(t)

	stdout, _, err := runCommandWithHTTP(t, rr, "--verify", "--json=valid,findings", "octo/api")

	require.ErrorIs(t, err, errSilent)
	var payload struct {
		Valid    bool `json:"valid"`
		Findings []struct {
			Category string `json:"category"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &payload))
	assert.False(t, payload.Valid)
	require.NotEmpty(t, payload.Findings)
	assert.Equal(t, "not-pinned", payload.Findings[0].Category)
	assertEmptyDir(t, cwd)
}

func TestVerifyRemote_NarrowedWorkflowPath(t *testing.T) {
	reg := &httpmock.Registry{}
	rr := newRemoteRepo(map[string]string{
		".github/workflows/ci.yml":       remoteWorkflow,
		".github/workflows/actions.lock": remoteLock("actions/checkout@v6"),
	}, reg)
	registerParity(reg)
	chdirEmpty(t)

	_, _, err := runCommandWithHTTP(t, rr, "--verify", "octo/api", ".github/workflows/ci.yml")

	require.NoError(t, err)
}

func TestVerifyRemote_Errors(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		args    []string
		noRepo  bool
		wantErr string
	}{
		{
			name:    "explicit workflow that does not exist",
			files:   map[string]string{".github/workflows/ci.yml": remoteWorkflow},
			args:    []string{"--verify", "octo/api", ".github/workflows/gone.yml"},
			wantErr: "gone.yml",
		},
		{
			name:    "repository without workflows",
			files:   map[string]string{"README.md": "hi"},
			args:    []string{"--verify", "octo/api"},
			wantErr: "no workflow files found",
		},
		{
			name:    "target not found",
			args:    []string{"--verify", "octo/api"},
			noRepo:  true,
			wantErr: "fetching metadata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			var rt http.RoundTripper = newRemoteRepo(tt.files, reg)
			if tt.noRepo {
				reg.Register(httpmock.REST("GET", `^/repos/octo/api$`), httpmock.StatusResponse(http.StatusNotFound))
				rt = reg
			}
			cwd := chdirEmpty(t)

			_, _, err := runCommandWithHTTP(t, rt, tt.args...)

			require.ErrorContains(t, err, tt.wantErr)
			assertEmptyDir(t, cwd)
		})
	}
}

func TestRemoteHostname(t *testing.T) {
	tests := []struct {
		name     string
		spec     repospec.Spec
		override string
		ghHost   string
		want     string
	}{
		{name: "spec host wins", spec: repospec.Spec{Host: "Acme.GHE.com"}, override: "x.com", ghHost: "y.com", want: "acme.ghe.com"},
		{name: "override next", override: "Other.ghe.com", ghHost: "y.com", want: "other.ghe.com"},
		{name: "GH_HOST next", ghHost: "Env.ghe.com", want: "env.ghe.com"},
		{name: "default", want: "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GH_CONFIG_DIR", t.TempDir())
			t.Setenv("GH_HOST", tt.ghHost)
			assert.Equal(t, tt.want, remoteHostname(&tt.spec, tt.override))
		})
	}
}

func TestShortSHA(t *testing.T) {
	assert.Equal(t, "0123456", shortSHA(remoteSHA))
	assert.Equal(t, "abc", shortSHA("abc"))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
