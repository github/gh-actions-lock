package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	paritySHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	parityMoved = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCheckPins_GraphQLParsesEachState(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	var gotQuery string
	var gotVars map[string]any
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{
		"data": {
			"a0": {"nameWithOwner": "actions/checkout", "databaseId": 2, "owner": {"databaseId": 1},
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+paritySHA+`"}},
			"a1": {"nameWithOwner": "typesafegithub/github-actions-typing", "databaseId": 3, "owner": {"databaseId": 4},
			       "commit": {"oid": "`+paritySHA+`"}},
			"a2": {"nameWithOwner": "octo/gone", "databaseId": 5, "owner": {"databaseId": 6}, "commit": null},
			"a3": {"nameWithOwner": "octo/retag", "databaseId": 7, "owner": {"databaseId": 8},
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+parityMoved+`"}},
			"a4": null
		},
		"errors": [
			{"type": "NOT_FOUND", "path": ["a2", "commit"], "message": "Could not resolve to a commit"},
			{"type": "NOT_FOUND", "path": ["a4"], "message": "Could not resolve to a Repository"}
		]
	}`, func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

	states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
		{Owner: "actions", Repo: "checkout", SHA: paritySHA, Tag: "v4.2.1"},
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "gone", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Tag: "v1.0.0"},
		{Owner: "octo", Repo: "deleted", SHA: paritySHA},
	})
	require.Len(t, states, 5)
	assert.Len(t, reg.Requests, 1, "one batched request for every pin")
	assert.NotContains(t, gotQuery, "action.yml", "parity must not fetch action blobs")
	assert.Equal(t, "v4.2.1^{commit}", gotVars["tag0"])
	assert.NotContains(t, gotVars, "tag1", "mutable refs are not tag-checked")

	assert.Equal(t, PinState{NameWithOwner: "actions/checkout", OwnerID: 1, RepoID: 2, CommitFound: true, TagOID: paritySHA}, states[0])
	assert.Equal(t, "typesafegithub/github-actions-typing", states[1].NameWithOwner)
	assert.True(t, states[1].CommitFound)
	assert.NoError(t, states[2].Err)
	assert.False(t, states[2].CommitFound)
	assert.True(t, states[3].CommitFound)
	assert.Equal(t, parityMoved, states[3].TagOID)
	assert.Error(t, states[4].Err, "null repository is inconclusive, not a verdict")
}

func TestCheckPins_GraphQLBatchesFifty(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	ok := func(req *http.Request) (*http.Response, error) {
		data := map[string]any{}
		for i := range pinCheckBatchSize {
			data[fmt.Sprintf("a%d", i)] = map[string]any{"nameWithOwner": "o/r", "commit": map[string]any{"oid": paritySHA}}
		}
		return httpmock.JSONResponse(map[string]any{"data": data})(req)
	}
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), ok)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), ok)

	pins := make([]PinCheck, pinCheckBatchSize+1)
	for i := range pins {
		pins[i] = PinCheck{Owner: "o", Repo: "r", SHA: paritySHA}
	}
	states := newTestClient(t, reg).CheckPins(context.Background(), pins)
	require.Len(t, states, len(pins))
	assert.Len(t, reg.Requests, 2)
	for _, s := range states {
		assert.True(t, s.CommitFound)
	}
}

func TestCheckPins_GraphQLSplitsOnBatchFailure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`a1:`), httpmock.StatusResponse(http.StatusBadGateway))
	for range 2 {
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
			"data": map[string]any{"a0": map[string]any{"nameWithOwner": "o/r", "commit": map[string]any{"oid": paritySHA}}},
		}))
	}

	states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
		{Owner: "o", Repo: "r", SHA: paritySHA},
		{Owner: "o", Repo: "r", SHA: paritySHA},
	})
	assert.Len(t, reg.Requests, 3)
	for _, s := range states {
		assert.NoError(t, s.Err)
		assert.True(t, s.CommitFound)
	}
}

func TestCheckPins_RESTOnly(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing$`), httpmock.JSONResponse(map[string]any{
		"full_name": "typesafegithub/github-actions-typing", "id": 3, "owner": map[string]any{"id": 4},
	}))
	reg.Register(httpmock.REST("GET", `^/repos/krzema12/github-actions-typing/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag$`), httpmock.JSONResponse(map[string]any{"full_name": "octo/retag"}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/v1.0.0$`), httpmock.JSONResponse(map[string]any{"sha": parityMoved}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/v2.0.0$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+parityMoved+`$`), httpmock.StatusResponse(http.StatusUnprocessableEntity))

	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}
	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Tag: "v1.0.0"},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Tag: "v2.0.0"},
		{Owner: "octo", Repo: "retag", SHA: parityMoved},
	})
	for _, s := range states {
		require.NoError(t, s.Err)
	}
	assert.Equal(t, PinState{NameWithOwner: "typesafegithub/github-actions-typing", OwnerID: 4, RepoID: 3, CommitFound: true}, states[0])
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, TagOID: parityMoved}, states[1], "a moved tag is not a missing commit")
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, TagOID: paritySHA}, states[2])
	assert.False(t, states[3].CommitFound)
	for _, req := range reg.Requests {
		assert.False(t, strings.HasSuffix(req.URL.Path, "/graphql"), "REST-only must not call GraphQL")
	}
}
