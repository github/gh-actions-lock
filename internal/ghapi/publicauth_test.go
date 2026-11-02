package ghapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/config"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGhConfig replaces go-gh's cached config read with hosts.
func stubGhConfig(t *testing.T, hosts string) {
	t.Helper()
	orig := config.Read
	t.Cleanup(func() { config.Read = orig })
	config.Read = func(*config.Config) (*config.Config, error) { return config.ReadFromString(hosts), nil }
}

// fakeGh points GH_PATH at a script that records its arguments in the
// returned file and prints out, or fails when out is empty.
func fakeGh(t *testing.T, out string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if out == "" {
		script += "exit 1\n"
	} else {
		script += "echo " + out + "\n"
	}
	gh := filepath.Join(dir, "gh")
	require.NoError(t, os.WriteFile(gh, []byte(script), 0o755))
	t.Setenv("GH_PATH", gh)
	return calls
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

func TestStoredDotcomToken(t *testing.T) {
	for _, tt := range []struct {
		name      string
		hosts     string
		keyring   string
		want      string
		wantCalls string
	}{
		{
			name:  "config token",
			hosts: "hosts:\n  github.com:\n    oauth_token: from-config\n",
			want:  "from-config",
		},
		{
			name:      "keyring token",
			hosts:     "hosts:\n  tenant.ghe.com:\n    oauth_token: tenant-config\n",
			keyring:   "from-keyring",
			want:      "from-keyring",
			wantCalls: "auth token --secure-storage --hostname github.com\n",
		},
		{
			name:      "no github.com login",
			hosts:     "hosts:\n  tenant.ghe.com:\n    oauth_token: tenant-config\n",
			wantCalls: "auth token --secure-storage --hostname github.com\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "env-gh-token")
			t.Setenv("GITHUB_TOKEN", "env-github-token")
			t.Setenv("GH_ENTERPRISE_TOKEN", "env-enterprise-token")
			stubGhConfig(t, tt.hosts)
			calls := fakeGh(t, tt.keyring)
			assert.Equal(t, tt.want, storedDotcomToken())
			assert.Equal(t, tt.wantCalls, readCalls(t, calls))
		})
	}
}

// withStoredLogin enables the stored-login lookup, which
// WithClientTransport turns off, and points it at token.
func withStoredLogin(t *testing.T, token string) ClientOption {
	t.Helper()
	hosts := ""
	if token != "" {
		hosts = "hosts:\n  github.com:\n    oauth_token: " + token + "\n"
	}
	stubGhConfig(t, hosts)
	t.Setenv("GH_PATH", filepath.Join(t.TempDir(), "missing-gh"))
	return func(c *clientConfig) { c.noStoredLogin = false }
}

func TestPublicFallbackCredential(t *testing.T) {
	for _, tt := range []struct {
		name     string
		env      string
		stored   string
		wantAuth string
	}{
		{name: "stored github.com login authenticates fallback", stored: "dotcom-secret", wantAuth: "token dotcom-secret"},
		{name: "token variable authenticates fallback", env: "ci-dotcom", wantAuth: "token ci-dotcom"},
		{name: "token variable takes precedence over stored login", env: "ci-dotcom", stored: "dotcom-secret", wantAuth: "token ci-dotcom"},
		{name: "no credential stays anonymous"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(DotcomTokenEnv, tt.env)
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(404))
			reg.Register(hostRequest("api.github.com", `repos/o/r$`),
				httpmock.JSONResponse(map[string]any{"visibility": "public", "id": 2, "owner": map[string]any{"id": 1}}))
			c, err := New("tenant.ghe.com", WithClientTransport(reg), withStoredLogin(t, tt.stored), func(cfg *clientConfig) {
				cfg.authToken = "tenant-only-secret"
			})
			require.NoError(t, err)
			selected, err := c.ForRepo(context.Background(), "o", "r")
			require.NoError(t, err)
			assert.Equal(t, "github.com", selected.Hostname)
			for _, req := range reg.Requests {
				auth := req.Header.Get("Authorization")
				if req.URL.Host == "api.github.com" {
					assert.Equal(t, tt.wantAuth, auth)
					assert.NotContains(t, auth, "tenant-only-secret")
					assert.Empty(t, req.Header.Get("Cookie"))
				} else {
					assert.Contains(t, auth, "tenant-only-secret")
					if tt.env != "" {
						assert.NotContains(t, auth, tt.env)
					}
				}
			}
		})
	}
}

func TestPublicFallbackTokenOnlyForTenants(t *testing.T) {
	t.Setenv(DotcomTokenEnv, "ci-dotcom")
	for _, host := range []string{"github.com", "github.example.com"} {
		stubGhConfig(t, "")
		calls := filepath.Join(t.TempDir(), "calls")
		t.Setenv("GH_PATH", filepath.Join(t.TempDir(), "missing-gh"))
		c, err := New(host, WithClientTransport(&httpmock.Registry{}), func(c *clientConfig) { c.noStoredLogin = false })
		require.NoError(t, err)
		assert.Nil(t, c.public, host)
		assert.Empty(t, readCalls(t, calls), host)
	}

	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(hostRequest("api.github.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	c, err := New("github.com", WithClientTransport(reg))
	require.NoError(t, err)
	_, _, err = c.RepoIDs(context.Background(), "o", "r")
	require.NoError(t, err)
	for _, req := range reg.Requests {
		assert.NotContains(t, req.Header.Get("Authorization"), "ci-dotcom", "github.com home host")
	}
}

func publicLimited(auth string) error {
	err := &api.HTTPError{StatusCode: http.StatusForbidden, Headers: http.Header{"X-Ratelimit-Remaining": {"0"}}}
	if auth == "" {
		return err
	}
	return &publicError{auth: auth, err: err}
}

func TestAnonymousFallbackLimited(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		want       bool
		wantReason string
	}{
		{"anonymous fallback", publicLimited(publicAnonymous), true, "github.com rate limit for anonymous public lookups exhausted"},
		{"anonymous fallback, wrapped", &UnknownOutcomeError{Cause: publicLimited(publicAnonymous)}, true, "github.com rate limit for anonymous public lookups exhausted"},
		{"stored login", publicLimited(publicStored), false, "GitHub API rate limit exhausted"},
		{"tenant or proxied", publicLimited(""), false, "GitHub API rate limit exhausted"},
		{"anonymous but not limited", &publicError{auth: publicAnonymous, err: &api.HTTPError{StatusCode: 500}}, false, "GitHub API returned 500 Internal Server Error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AnonymousFallbackLimited(tt.err))
			reason, limited := Outage(tt.err)
			assert.Equal(t, tt.wantReason, reason)
			assert.Equal(t, tt.want || strings.Contains(reason, "rate limit"), limited)
			if tt.want {
				assert.Contains(t, Brief(tt.err), "gh auth login --hostname github.com")
				assert.Contains(t, Brief(tt.err), DotcomTokenEnv)
			} else {
				assert.NotContains(t, Brief(tt.err), "gh auth login")
			}
		})
	}
}

func TestPublicFallbackCredentialRejected(t *testing.T) {
	t.Setenv("GH_TOKEN", "tenant-token")
	u, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/o/r", nil)
	for _, tt := range []struct {
		mode, want string
	}{
		{publicStored, "GitHub rejected the stored github.com credential used for public lookups (401); run `gh auth login --hostname github.com`"},
		{publicEnv, "GitHub rejected the github.com token in GH_ACTIONS_LOCK_GITHUB_COM_TOKEN (401); check it, or unset it"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			err := &publicError{auth: tt.mode, err: &api.HTTPError{StatusCode: http.StatusUnauthorized, RequestURL: u.URL}}
			got := ConfigError(err)
			require.Error(t, got)
			assert.Equal(t, tt.want, got.Error())
		})
	}
}

func TestPublicTransportKeepsTokenOnAPIHost(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(hostRequest("api.github.com", `repos/o/r/tarball$`), func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://codeload.github.com/o/r/tar"}}, Body: http.NoBody, Request: req}, nil
	})
	reg.Register(hostRequest("codeload.github.com", `o/r/tar$`), httpmock.StatusResponse(http.StatusOK))
	reg.Register(hostRequest("api.github.com", `repos/o/r$`), httpmock.StatusResponse(http.StatusOK))
	client := &http.Client{Transport: publicTransport{inner: reg, token: "dotcom-secret"}}

	for _, u := range []string{"https://api.github.com/repos/o/r/tarball", "https://api.github.com/repos/o/r"} {
		resp, err := client.Get(u)
		require.NoError(t, err)
		resp.Body.Close()
	}
	require.Len(t, reg.Requests, 3)
	assert.Equal(t, "token dotcom-secret", reg.Requests[0].Header.Get("Authorization"))
	assert.Empty(t, reg.Requests[1].Header.Get("Authorization"), "redirect to another host")
	assert.Equal(t, "token dotcom-secret", reg.Requests[2].Header.Get("Authorization"))
}
