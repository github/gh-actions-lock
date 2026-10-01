package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hostRequest(host, path string) httpmock.Matcher {
	return func(req *http.Request) bool {
		return req.URL.Host == host && httpmock.REST("GET", path)(req)
	}
}

func TestProximaRepositorySelection(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		pin        string
		visibility string
		wantHost   string
	}{
		{name: "tenant repository shadows dotcom", status: 200, wantHost: "tenant.ghe.com"},
		{name: "repository 404 permits public fallback", status: 404, visibility: "public", wantHost: "github.com"},
		{name: "private dotcom repository rejected", status: 404, visibility: "private"},
		{name: "missing visibility rejected", status: 404},
		{name: "unauthorized does not fall back", status: 401},
		{name: "forbidden does not fall back", status: 403},
		{name: "rate limit does not fall back", status: 429},
		{name: "server failure does not fall back", status: 500},
		{name: "transport failure does not fall back", status: -1},
		{name: "pinned tenant 404 cannot change hosts", status: 404, pin: "tenant.ghe.com"},
		{name: "pinned dotcom bypasses tenant namesake", pin: "github.com", visibility: "public", wantHost: "github.com"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.pin != "github.com" {
				response := httpmock.StatusResponse(tt.status)
				if tt.status == 200 {
					response = httpmock.JSONResponse(map[string]any{"id": 20, "owner": map[string]any{"id": 10}})
				} else if tt.status == -1 {
					response = func(*http.Request) (*http.Response, error) { return nil, errors.New("network unavailable") }
				}
				reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), response)
			}
			if tt.pin == "github.com" || tt.status == 404 && tt.pin == "" {
				reg.Register(hostRequest("api.github.com", `repos/o/r$`),
					httpmock.JSONResponse(map[string]any{"visibility": tt.visibility, "id": 2, "owner": map[string]any{"id": 1}}))
			}
			c, err := New("tenant.ghe.com", WithClientTransport(reg), func(cfg *clientConfig) {
				cfg.authToken = "tenant-only-secret"
			})
			require.NoError(t, err)
			if tt.pin != "" {
				require.NoError(t, c.PinHost("o", "r", tt.pin))
			}
			selected, err := c.ForRepo(context.Background(), "o", "r")
			if tt.wantHost == "" {
				require.Error(t, err)
				assert.Nil(t, selected)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantHost, selected.Hostname)
				again, err := c.ForRepo(context.Background(), "O", "R")
				require.NoError(t, err)
				assert.Same(t, selected, again)
				ownerID, repoID, err := c.RepoIDs(context.Background(), "o", "r")
				require.NoError(t, err)
				if tt.wantHost == "github.com" {
					assert.EqualValues(t, 1, ownerID)
					assert.EqualValues(t, 2, repoID)
				} else {
					assert.EqualValues(t, 10, ownerID)
					assert.EqualValues(t, 20, repoID)
				}
			}
			for _, req := range reg.Requests {
				if req.URL.Host == "api.github.com" {
					assert.Empty(t, req.Header.Get("Authorization"))
					assert.Empty(t, req.Header.Get("Cookie"))
				} else {
					assert.Contains(t, req.Header.Get("Authorization"), "tenant-only-secret")
				}
			}
		})
	}
}

func TestProximaMissingRefDoesNotFallBack(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	reg.Register(httpmock.GraphQLForRepo("o", "r"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{"nameWithOwner": "o/r", "object": nil}},
	}))
	c, err := New("tenant.ghe.com", WithClientTransport(reg))
	require.NoError(t, err)
	results := c.ResolveActionFiles(context.Background(), []ActionFileRequest{{Owner: "o", Repo: "r", Ref: "missing"}})
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	for _, req := range reg.Requests {
		assert.Equal(t, "api.tenant.ghe.com", req.URL.Host)
	}
}

func TestPinnedHostBoundaries(t *testing.T) {
	c, err := New("tenant.ghe.com", WithClientTransport(&httpmock.Registry{}))
	require.NoError(t, err)
	require.NoError(t, c.PinHost("o", "r", "github.com"))
	require.ErrorContains(t, c.PinHost("O", "R", "tenant.ghe.com"), "conflicting")
	require.ErrorContains(t, c.PinHost("o", "else", "other.ghe.com"), "not the selected host")
	require.ErrorContains(t, c.PinHost("o", "else", "evil.example"), "not the selected host")

	for _, host := range []string{"github.com", "github.example.com", "evilghe.com", "tenant.ghe.com.evil.example"} {
		c, err := New(host, WithClientTransport(&httpmock.Registry{}))
		require.NoError(t, err)
		assert.Nil(t, c.local, host)
		selected, err := c.ForRepo(context.Background(), "o", "r")
		require.NoError(t, err)
		assert.Same(t, c, selected)
	}
}

func TestProximaContentErrorsAreNotLeafActions(t *testing.T) {
	for _, status := range []int{403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(hostRequest("api.github.com", `repos/o/r$`),
				httpmock.JSONResponse(map[string]any{"visibility": "public", "id": 2, "owner": map[string]any{"id": 1}}))
			reg.Register(hostRequest("api.github.com", `repos/o/r/commits/v1$`),
				httpmock.JSONResponse(map[string]any{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}))
			reg.Register(hostRequest("api.github.com", `repos/o/r/contents/action.yml$`), httpmock.StatusResponse(status))
			c, err := New("tenant.ghe.com", WithClientTransport(reg))
			require.NoError(t, err)
			require.NoError(t, c.PinHost("o", "r", "github.com"))
			result := c.ResolveActionFiles(context.Background(), []ActionFileRequest{{Owner: "o", Repo: "r", Ref: "v1"}})
			require.Len(t, result, 1)
			var httpErr *api.HTTPError
			require.ErrorAs(t, result[0].Err, &httpErr)
			assert.Equal(t, status, httpErr.StatusCode)
			assert.Empty(t, result[0].ActionYML)
		})
	}
}

func TestProximaPartialGraphQLErrorDoesNotSucceed(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	reg.Register(httpmock.GraphQLForRepo("o", "r"), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{"a0": map[string]any{
			"nameWithOwner": "o/r", "object": map[string]any{"oid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "file": nil},
		}},
		"errors": []any{map[string]any{"type": "FORBIDDEN", "message": "content access denied", "path": []string{"a0", "object", "file"}}},
	}))
	c, err := New("tenant.ghe.com", WithClientTransport(reg))
	require.NoError(t, err)
	result := c.ResolveActionFiles(context.Background(), []ActionFileRequest{{Owner: "o", Repo: "r", Ref: "v1"}})
	require.Len(t, result, 1)
	require.ErrorContains(t, result[0].Err, "content access denied")
	assert.Empty(t, result[0].CommitOID)
}
