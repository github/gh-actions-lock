package ghapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoMetadata_FullNameAfterRedirect(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST(http.MethodGet, `^/repos/octo/old$`),
		httpmock.JSONResponse(map[string]any{
			"full_name":      "acme/new",
			"default_branch": "trunk",
			"visibility":     "public",
		}),
	)
	c, err := New("github.com", WithClientTransport(reg))
	require.NoError(t, err)

	got, err := c.RepoMetadata(context.Background(), "octo", "old")

	require.NoError(t, err)
	assert.Equal(t, "acme/new", got.FullName, "canonical name is exposed so callers can see a rename")
	assert.Equal(t, "trunk", got.DefaultBranch)
	reg.Verify(t)
}
