package ghapi

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrgHostedRunnerNames_Success(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "orgs/my-org/actions/hosted-runners"),
		httpmock.JSONResponse(map[string]any{
			"total_count": 2,
			"runners": []map[string]any{
				{"name": "ubuntu-latest-xl"},
				{"name": "ubuntu-latest-2xl"},
			},
		}),
	)
	c, err := New("github.com", WithClientTransport(reg))
	require.NoError(t, err)

	names, err := c.OrgHostedRunnerNames(context.Background(), "my-org")
	require.NoError(t, err)
	assert.Equal(t, []string{"ubuntu-latest-xl", "ubuntu-latest-2xl"}, names)
}

func TestOrgHostedRunnerNames_403_ReturnsNil(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "orgs/my-org/actions/hosted-runners"),
		httpmock.StatusResponse(403),
	)
	c, err := New("github.com", WithClientTransport(reg))
	require.NoError(t, err)

	names, err := c.OrgHostedRunnerNames(context.Background(), "my-org")
	assert.NoError(t, err)
	assert.Nil(t, names)
}
