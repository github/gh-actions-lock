package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
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
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+paritySHA+`"}, "tagRef": {}},
			"a1": {"nameWithOwner": "typesafegithub/github-actions-typing", "databaseId": 3, "owner": {"databaseId": 4},
			       "commit": {"oid": "`+paritySHA+`"}},
			"a2": {"nameWithOwner": "octo/gone", "databaseId": 5, "owner": {"databaseId": 6}, "commit": null},
			"a3": {"nameWithOwner": "octo/retag", "databaseId": 7, "owner": {"databaseId": 8},
			       "commit": {"oid": "`+paritySHA+`"}, "tag": {"oid": "`+parityMoved+`"}, "tagRef": {}},
			"a4": null,
			"a5": {"nameWithOwner": "octo/branch", "databaseId": 9, "owner": {"databaseId": 10},
			       "commit": {"oid": "`+paritySHA+`"}, "head": {"oid": "`+parityMoved+`"}, "headRef": {}}
		},
		"errors": [
			{"type": "NOT_FOUND", "path": ["a2", "commit"], "message": "Could not resolve to a commit"},
			{"type": "NOT_FOUND", "path": ["a4"], "message": "Could not resolve to a Repository"}
		]
	}`, func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

	states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
		{Owner: "actions", Repo: "checkout", SHA: paritySHA, Head: "v4.2.1"},
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "gone", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v1.0.0"},
		{Owner: "octo", Repo: "deleted", SHA: paritySHA},
		{Owner: "octo", Repo: "branch", SHA: paritySHA, Head: "v2.0.0"},
	})
	require.Len(t, states, 6)
	assert.Len(t, reg.Requests, 1, "one batched request for every pin")
	assert.NotContains(t, gotQuery, "action.yml", "parity must not fetch action blobs")
	assert.Equal(t, "refs/tags/v4.2.1^{commit}", gotVars["tag0"])
	assert.Equal(t, "refs/heads/v4.2.1^{commit}", gotVars["head0"])
	assert.NotContains(t, gotVars, "tag1", "a pin with no ref looks none up")

	assert.Equal(t, PinState{NameWithOwner: "actions/checkout", OwnerID: 1, RepoID: 2, CommitFound: true, HeadOID: paritySHA, HeadIsTag: true}, states[0])
	assert.Equal(t, "typesafegithub/github-actions-typing", states[1].NameWithOwner)
	assert.True(t, states[1].CommitFound)
	assert.NoError(t, states[2].Err)
	assert.False(t, states[2].CommitFound)
	assert.True(t, states[3].CommitFound)
	assert.Equal(t, parityMoved, states[3].HeadOID)
	assert.True(t, states[3].HeadIsTag)
	assert.NoError(t, states[4].Err)
	assert.True(t, states[4].RepoMissing, "a NOT_FOUND repository is a verdict the runner shares")
	assert.False(t, states[4].ViaFallback)
	assert.Equal(t, parityMoved, states[5].HeadOID)
	assert.False(t, states[5].HeadIsTag, "a branch named like a release is not a tag")
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

func TestCheckPins_GraphQLSplitsOnQueryLevelError(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`a1:`), httpmock.JSONResponse(map[string]any{
		"errors": []map[string]any{{"message": "query has cost too high", "type": "MAX_NODE_LIMIT_EXCEEDED"}},
	}))
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

// TestCheckPins_OutageIsNotSplit: an outage or rate limit rejects every half
// of a batch alike, and the transport already retried it, so a full batch
// costs one request rather than fanning out into 2n-1.
func TestCheckPins_OutageIsNotSplit(t *testing.T) {
	tests := map[string]httpmock.Responder{
		"502":                  httpmock.StatusResponse(http.StatusBadGateway),
		"secondary rate limit": httpmock.StatusResponseWithHeaders(http.StatusForbidden, map[string]string{"Retry-After": "60"}),
		"graphql rate limited": httpmock.JSONResponse(map[string]any{
			"errors": []map[string]any{{"message": "API rate limit exceeded", "type": "RATE_LIMITED"}},
		}),
	}
	for name, resp := range tests {
		t.Run(name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), resp)

			pins := make([]PinCheck, pinCheckBatchSize)
			for i := range pins {
				pins[i] = PinCheck{Owner: "o", Repo: "r", SHA: paritySHA}
			}
			states := newTestClient(t, reg).CheckPins(context.Background(), pins)
			assert.Len(t, reg.Requests, 1)
			require.Len(t, states, len(pins))
			for _, s := range states {
				assert.True(t, IsTransient(s.Err), "%v", s.Err)
			}
		})
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
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1.0.0", parityMoved)))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/git/ref/tags/v2\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v2.0.0", paritySHA)))
	reg.Register(httpmock.REST("GET", `^/repos/octo/retag/commits/`+parityMoved+`$`), httpmock.StatusResponse(http.StatusUnprocessableEntity))
	reg.Register(httpmock.REST("GET", `^/repos/octo/deleted$`), httpmock.StatusResponse(http.StatusNotFound))

	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}
	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "krzema12", Repo: "github-actions-typing", SHA: paritySHA},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v1.0.0"},
		{Owner: "octo", Repo: "retag", SHA: paritySHA, Head: "v2.0.0"},
		{Owner: "octo", Repo: "retag", SHA: parityMoved},
		{Owner: "octo", Repo: "deleted", SHA: paritySHA},
	})
	for _, s := range states {
		require.NoError(t, s.Err)
	}
	assert.Equal(t, PinState{NameWithOwner: "typesafegithub/github-actions-typing", OwnerID: 4, RepoID: 3, CommitFound: true}, states[0])
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, HeadOID: parityMoved, HeadIsTag: true}, states[1], "a moved tag is not a missing commit")
	assert.Equal(t, PinState{NameWithOwner: "octo/retag", CommitFound: true, HeadOID: paritySHA, HeadIsTag: true}, states[2])
	assert.False(t, states[3].CommitFound)
	assert.Equal(t, PinState{RepoMissing: true, ViaFallback: true}, states[4])
	for _, req := range reg.Requests {
		assert.False(t, strings.HasSuffix(req.URL.Path, "/graphql"), "REST-only must not call GraphQL")
	}
}

func TestPinCheckRefs(t *testing.T) {
	tests := []struct {
		name string
		pc   PinCheck
		want []string
	}{
		{name: "no ref", pc: PinCheck{}},
		{name: "head tries tag then branch", pc: PinCheck{Head: "main"}, want: []string{"refs/tags/main", "refs/heads/main"}},
		{name: "a full ref name is never used as-is", pc: PinCheck{Head: "refs/tags/v1"}, want: []string{"refs/tags/refs/tags/v1", "refs/heads/refs/tags/v1"}},
		{name: "a pull request ref is not a tag or branch", pc: PinCheck{Head: "pull/1/head"}, want: []string{"refs/tags/pull/1/head", "refs/heads/pull/1/head"}},
		{name: "revision syntax is never looked up", pc: PinCheck{Head: "main~1"}},
		{name: "reflog syntax is never looked up", pc: PinCheck{Head: "v1@{1}"}},
		{name: "a describe name is never looked up", pc: PinCheck{Head: "v6.1.0-1-g373d742f768e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.pc.refs())
		})
	}
}

func TestCheckPins_RefLookup(t *testing.T) {
	t.Run("graphql prefers the tag over a same-named branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotQuery string
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": {"oid": "`+paritySHA+`"}, "tagRef": {}, "head": {"oid": "`+parityMoved+`"}, "headRef": {}},
			"a1": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": null, "head": {"oid": "`+parityMoved+`"}, "headRef": {}}}}`,
			func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"},
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"},
		})
		require.NoError(t, states[0].Err)
		require.NoError(t, states[1].Err)
		assert.Contains(t, gotQuery, "tag: object(expression: $tag0)")
		assert.Contains(t, gotQuery, "head: object(expression: $head0)")
		assert.Equal(t, "refs/tags/v1^{commit}", gotVars["tag0"])
		assert.Equal(t, "refs/heads/v1^{commit}", gotVars["head0"])
		assert.Equal(t, paritySHA, states[0].HeadOID)
		assert.Equal(t, parityMoved, states[1].HeadOID, "a ref falls back to a branch like the Actions backend resolver")
	})

	t.Run("rest falls back from tag to branch", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		for range 3 {
			reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		}
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusNotFound))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/main$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/heads/main", parityMoved)))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v2\.0\.0$`), httpmock.StatusResponse(http.StatusNotFound))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/v2\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/heads/v2.0.0", parityMoved)))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/pull/1/head$`), httpmock.StatusResponse(http.StatusNotFound))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/pull/1/head$`), httpmock.StatusResponse(http.StatusNotFound))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "main"},
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "pull/1/head"},
			{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v2.0.0"},
		})
		for _, s := range states {
			require.NoError(t, s.Err)
		}
		assert.Equal(t, parityMoved, states[0].HeadOID)
		assert.False(t, states[0].HeadIsTag)
		assert.Empty(t, states[1].HeadOID, "a pull request head is not a tag or branch")
		assert.Equal(t, parityMoved, states[2].HeadOID)
		assert.False(t, states[2].HeadIsTag, "a branch named like a release is not a tag")
	})
}

// TestCheckPins_ExactRefName: a recorded ref counts only if a tag or branch
// exists by exactly that name.
func TestCheckPins_ExactRefName(t *testing.T) {
	t.Run("graphql ignores an expression that names no ref", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotQuery string
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": {"oid": "`+paritySHA+`"}, "head": {"oid": "`+paritySHA+`"}, "tagRef": null, "headRef": null}}}`,
			func(q string, v map[string]any) { gotQuery, gotVars = q, v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v9"}})
		require.NoError(t, states[0].Err)
		assert.Contains(t, gotQuery, "tagRef: ref(qualifiedName: $tagRef0)")
		assert.Contains(t, gotQuery, "headRef: ref(qualifiedName: $headRef0)")
		assert.Equal(t, "refs/tags/v9", gotVars["tagRef0"])
		assert.Equal(t, "refs/heads/v9", gotVars["headRef0"])
		assert.Empty(t, states[0].HeadOID, "no such tag or branch")
	})

	t.Run("graphql never looks up a describe name", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		var gotVars map[string]any
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"}}}}`,
			func(_ string, v map[string]any) { gotVars = v }))

		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v6.1.0-1-g373d742f768e"}})
		require.NoError(t, states[0].Err)
		assert.NotContains(t, gotVars, "tag0")
		assert.NotContains(t, gotVars, "tagRef0")
		assert.Empty(t, states[0].HeadOID)
	})

	t.Run("rest reads the exact ref", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1.0.0", paritySHA)))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"}})
		require.NoError(t, states[0].Err)
		assert.Equal(t, paritySHA, states[0].HeadOID)
		assert.True(t, states[0].CommitFound, "a ref at the commit proves it exists")
	})

	t.Run("rest ignores a name that is not an exact ref", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v9$`), httpmock.StatusResponse(http.StatusNotFound))
		// git/ref answers a name that only prefixes refs with an array.
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/heads/v9$`),
			httpmock.JSONResponse([]any{httpmock.GitRefResponse("refs/heads/v9/x", paritySHA)}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v9"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
		for _, req := range reg.Requests {
			assert.NotContains(t, req.URL.Path, "/commits/refs", "commits/{ref} would evaluate the name")
		}
	})

	t.Run("rest never looks up a describe name", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v6.1.0-1-g373d742f768e"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})

	t.Run("rest peels an annotated tag only once the ref exists", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1\.0\.0$`), httpmock.JSONResponse(map[string]any{
			"ref": "refs/tags/v1.0.0", "object": map[string]any{"sha": parityMoved, "type": "tag"},
		}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/refs/tags/v1\.0\.0$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1.0.0"}})
		require.NoError(t, states[0].Err)
		assert.Equal(t, paritySHA, states[0].HeadOID, "the tag object peels to its commit")
		assert.True(t, states[0].CommitFound)
	})

	t.Run("rest ref lookup failure is inconclusive", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusBadGateway))

		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "main"}})
		require.Error(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})
}

func TestTagExists(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "authenticated", true: "dependabot proxy"}[proxy], func(t *testing.T) {
			if proxy {
				t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
			}
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/v1", paritySHA)))
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/main$`), httpmock.StatusResponse(http.StatusNotFound))
			// git/ref answers a name that only prefixes refs with an array.
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v2$`),
				httpmock.JSONResponse([]any{httpmock.GitRefResponse("refs/tags/v2/x", paritySHA)}))
			reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/down$`), httpmock.StatusResponse(http.StatusBadGateway))

			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}
			ctx := context.Background()
			for name, want := range map[string]bool{"v1": true, "main": false, "v2": false} {
				got, err := c.TagExists(ctx, "o", "r", name)
				require.NoError(t, err, name)
				assert.Equal(t, want, got, name)
			}
			_, err := c.TagExists(ctx, "o", "r", "down")
			assert.Error(t, err, "a failed lookup is not a missing tag")
		})
	}
}

// TestCheckPins_UnpeelableTagShadowsBranch: a tag that exists but doesn't
// peel to a commit still wins over a same-named branch, as in the Actions
// backend; the ref resolves to no commit rather than the branch's.
func TestCheckPins_UnpeelableTagShadowsBranch(t *testing.T) {
	t.Run("graphql", func(t *testing.T) {
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.GraphQLQuery(`{"data": {
			"a0": {"nameWithOwner": "o/r", "commit": {"oid": "`+paritySHA+`"},
				"tag": null, "tagRef": {}, "head": {"oid": "`+paritySHA+`"}, "headRef": {}}}}`, func(string, map[string]any) {}))
		states := newTestClient(t, reg).CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
	})
	t.Run("rest", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		reg := &httpmock.Registry{}
		defer reg.Verify(t)
		reg.Register(httpmock.REST("GET", `^/repos/o/r$`), httpmock.JSONResponse(map[string]any{"full_name": "o/r"}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/git/ref/tags/v1$`), httpmock.JSONResponse(map[string]any{
			"ref": "refs/tags/v1", "object": map[string]any{"sha": parityMoved, "type": "tag"},
		}))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/refs/tags/v1$`), httpmock.StatusResponse(http.StatusUnprocessableEntity))
		reg.Register(httpmock.REST("GET", `^/repos/o/r/commits/`+paritySHA+`$`), httpmock.JSONResponse(map[string]any{"sha": paritySHA}))
		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		states := c.CheckPins(context.Background(), []PinCheck{{Owner: "o", Repo: "r", SHA: paritySHA, Head: "v1"}})
		require.NoError(t, states[0].Err)
		assert.Empty(t, states[0].HeadOID)
		for _, req := range reg.Requests {
			assert.NotContains(t, req.URL.Path, "/git/ref/heads/", "an existing tag shadows the branch")
		}
	})
}

func TestCheckPins_Batching(t *testing.T) {
	t.Run("small concurrent batches", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.RegisterReusable(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{"data": map[string]any{}}))
		pins := make([]PinCheck, 2*pinCheckBatchSize+1)
		for i := range pins {
			pins[i] = PinCheck{Owner: "octo", Repo: fmt.Sprintf("r%d", i), SHA: paritySHA}
		}

		states := newTestClient(t, reg).CheckPins(context.Background(), pins)

		assert.Len(t, states, len(pins))
		assert.Len(t, reg.Requests, 3)
	})

	t.Run("one large batch at a time behind the dependabot proxy", func(t *testing.T) {
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
		size, workers := pinCheckBatching()
		assert.Equal(t, proxyPinCheckBatchSize, size)
		assert.Equal(t, 1, workers)
	})
}

func TestCheckPins_IPAllowListFallback(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.RegisterReusable(anyRequest, ipAllowListAPI("acme", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			w.WriteHeader(http.StatusOK)
		case "/repos/acme/widget":
			writeJSON(w, map[string]any{"full_name": "acme/widget", "id": 7, "owner": map[string]any{"id": 6}})
		case "/repos/acme/widget/git/ref/tags/v1.0.0":
			writeJSON(w, httpmock.GitRefResponse("refs/tags/v1.0.0", paritySHA))
		default:
			http.NotFound(w, r)
		}
	}))
	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}

	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "acme", Repo: "widget", SHA: paritySHA, Head: "v1.0.0"},
		{Owner: "acme", Repo: "widget", SHA: parityMoved},
	})
	require.Len(t, states, 2)
	require.NoError(t, states[0].Err)
	assert.Equal(t, "acme/widget", states[0].NameWithOwner)
	assert.Equal(t, int64(6), states[0].OwnerID)
	assert.Equal(t, int64(7), states[0].RepoID)
	assert.True(t, states[0].CommitFound)
	assert.Equal(t, paritySHA, states[0].HeadOID)
	assert.True(t, states[0].HeadIsTag)
	require.NoError(t, states[1].Err)
	assert.False(t, states[1].CommitFound, "a missing commit is a verdict, not an error")
}

func TestCheckPins_GraphQLIPAllowListFallback(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.GraphQL(`commit: object\(oid`), httpmock.JSONResponse(map[string]any{
		"data": map[string]any{
			"a0": nil,
			"a1": map[string]any{"nameWithOwner": "acme/private", "commit": map[string]any{"oid": parityMoved}},
		},
		"errors": []api.GraphQLErrorItem{{Type: "FORBIDDEN", Message: ipAllowListDeniedMessage("acme"), Path: []any{"a0"}}},
	}))
	reg.RegisterReusable(anyRequest, ipAllowListAPI("acme", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			w.WriteHeader(http.StatusOK)
		case "/repos/acme/widget":
			writeJSON(w, map[string]any{"full_name": "acme/widget", "id": 7, "owner": map[string]any{"id": 6}})
		case "/repos/acme/widget/git/ref/tags/v1":
			writeJSON(w, httpmock.GitRefResponse("refs/tags/v1", paritySHA))
		default:
			t.Errorf("unexpected anonymous read: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}
	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "acme", Repo: "widget", SHA: paritySHA, Head: "v1"},
		{Owner: "acme", Repo: "private", SHA: parityMoved},
	})
	require.Len(t, states, 2)
	require.NoError(t, states[0].Err)
	assert.Equal(t, "acme/widget", states[0].NameWithOwner)
	assert.Equal(t, int64(7), states[0].RepoID)
	assert.Equal(t, int64(6), states[0].OwnerID)
	assert.True(t, states[0].CommitFound)
	assert.Equal(t, paritySHA, states[0].HeadOID)
	require.NoError(t, states[1].Err)
	assert.Equal(t, "acme/private", states[1].NameWithOwner)
	assert.True(t, states[1].CommitFound)
	assert.NotEmpty(t, reg.Requests[0].Header.Get("Authorization"))
}

// A publicly visible organization does not make every repository readable.
func TestCheckPins_IPAllowListFallbackFailsClosed(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	var probes int
	reg.RegisterReusable(anyRequest, ipAllowListAPI("acme", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/acme" {
			probes++
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}

	states := c.CheckPins(context.Background(), []PinCheck{
		{Owner: "acme", Repo: "private", SHA: paritySHA},
	})
	require.Len(t, states, 1)
	assert.Equal(t, 1, probes, "the probe ran and the repository read anonymously failed")
	code, ok := StatusCode(states[0].Err)
	require.True(t, ok, "want the policy refusal, got %v", states[0].Err)
	assert.Equal(t, http.StatusForbidden, code)
	assert.False(t, states[0].RepoMissing, "an unreadable repository is not a verdict")
	assert.False(t, states[0].CommitFound)
	assert.Empty(t, states[0].NameWithOwner)
}

func TestCheckPins_NoFallbackForOtherDenials(t *testing.T) {
	tests := []struct {
		name      string
		response  httpmock.Responder
		hasStatus bool
	}{
		{"ordinary forbidden", httpmock.StatusResponse(http.StatusForbidden), true},
		{"untyped IP-like GraphQL message", httpmock.JSONResponse(map[string]any{
			"data":   map[string]any{"a0": nil},
			"errors": []map[string]any{{"message": ipAllowListDeniedMessage("acme"), "path": []any{"a0"}}},
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetAnonFallbackState()
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.Register(httpmock.GraphQL(`commit: object\(oid`), tt.response)
			reg.RegisterReusable(anyRequest, unexpectedAnonRequest(t))
			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}

			states := c.CheckPins(context.Background(), []PinCheck{
				{Owner: "acme", Repo: "widget", SHA: paritySHA},
			})
			require.Len(t, states, 1)
			require.Error(t, states[0].Err)
			assert.False(t, states[0].RepoMissing)
			code, ok := StatusCode(states[0].Err)
			assert.Equal(t, tt.hasStatus, ok)
			if tt.hasStatus {
				assert.Equal(t, http.StatusForbidden, code)
			}
		})
	}
}

func unexpectedAnonRequest(t *testing.T) httpmock.Responder {
	t.Helper()
	return func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected anonymous request: %s %s", req.Method, req.URL.Path)
		return nil, fmt.Errorf("unexpected anonymous request")
	}
}
