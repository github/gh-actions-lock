package ghapi

// Dependabot's updater never holds real GitHub tokens: it sees credential
// metadata only, so its lockfile Env.build sets GH_TOKEN to the placeholder
// "x-access-token" plus GH_ACTIONS_LOCK_DEPENDABOT_PROXY=1, and its proxy
// replaces auth on every api.github.com and api.<tenant>.ghe.com request.
// For a *.ghe.com job the proxy authenticates api.github.com with a
// tenant-scoped service identity header, not Authorization, so the CLI must
// send github.com requests with no credential of its own. The only run with
// a real GH_TOKEN is the local dry-run script, whose home host is github.com.

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envTransport injects reg but leaves go-gh to resolve the home token from
// GH_TOKEN, as in a Dependabot run.
func envTransport(reg http.RoundTripper) ClientOption {
	return func(c *clientConfig) {
		c.transport = reg
		c.logIgnore = true
	}
}

// availableCredentials puts a github.com credential everywhere the CLI could
// look, so a test fails if any of them is used.
func availableCredentials(t *testing.T) string {
	t.Helper()
	t.Setenv(DotcomTokenEnv, "ci-dotcom")
	stubGhConfig(t, "hosts:\n  github.com:\n    oauth_token: stored-dotcom\n")
	return fakeGh(t, buildFakeGh(t), "keyring-dotcom")
}

func TestDependabotContract(t *testing.T) {
	t.Run("hosted tenant job sends no github.com credential", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "x-access-token")
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		calls := availableCredentials(t)
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(404))
		reg.Register(hostRequest("api.github.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{
			"visibility": "public", "id": 2, "owner": map[string]any{"id": 1},
		}))

		c, err := New("tenant.ghe.com", envTransport(reg))
		require.NoError(t, err)
		// Under the proxy, REST reads go through the raw HTTP client.
		c.local.anonHTTP = &http.Client{Transport: reg}
		selected, err := c.ForRepo(context.Background(), "o", "r")
		require.NoError(t, err)
		assert.Equal(t, "github.com", selected.Hostname)
		assert.Empty(t, readCalls(t, calls), "ran gh under the Dependabot proxy")
		for _, req := range reg.Requests {
			if req.URL.Host == "api.github.com" {
				assert.Empty(t, req.Header.Get("Authorization"))
			}
		}
	})

	t.Run("hosted rate-limit wording is unchanged", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		reg.Register(hostRequest("api.github.com", `repos/o/r$`), func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Header: http.Header{"X-Ratelimit-Remaining": {"0"}}, Body: http.NoBody, Request: req}, nil
		})
		c, err := New("tenant.ghe.com", WithClientTransport(reg))
		require.NoError(t, err)
		require.NoError(t, c.PinHost("o", "r", "github.com"))
		_, err = c.ForRepo(context.Background(), "o", "r")
		require.Error(t, err)
		assert.False(t, AnonymousFallbackLimited(err))
		reason, limited := Outage(err)
		assert.Equal(t, "GitHub API rate limit exhausted", reason)
		assert.True(t, limited)
		assert.Equal(t, "GitHub returned 403 Forbidden", Brief(err))
	})

	t.Run("hosted github.com job has no fallback", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "x-access-token")
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		calls := availableCredentials(t)
		c, err := New("github.com", envTransport(&httpmock.Registry{}))
		require.NoError(t, err)
		assert.Nil(t, c.public)
		assert.Empty(t, readCalls(t, calls))
	})

	t.Run("dry-run github.com job uses only GH_TOKEN", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "dry-run-token")
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "")
		calls := availableCredentials(t)
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(hostRequest("api.github.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))

		c, err := New("github.com", envTransport(reg))
		require.NoError(t, err)
		_, _, err = c.RepoIDs(context.Background(), "o", "r")
		require.NoError(t, err)
		assert.Empty(t, readCalls(t, calls))
		for _, req := range reg.Requests {
			assert.Equal(t, "token dry-run-token", req.Header.Get("Authorization"))
		}
	})
}
