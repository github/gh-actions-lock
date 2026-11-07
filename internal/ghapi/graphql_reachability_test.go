package ghapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildBranchCompareQuery(t *testing.T) {
	branches := []BranchHead{
		{Name: "main", SHA: "aaa"},
		{Name: "release/v4", SHA: "bbb"},
	}

	query, vars := buildBranchCompareQuery("o", "r", "targetsha", branches)

	for _, want := range []string{
		"$owner: String!", "$name: String!", "$sha: String!",
		"$ref0: String!", "$ref1: String!",
		"b0: ref(qualifiedName: $ref0)",
		"b1: ref(qualifiedName: $ref1)",
		"compare(headRef: $sha)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}

	if vars["owner"] != "o" || vars["name"] != "r" || vars["sha"] != "targetsha" {
		t.Fatalf("unexpected base vars: %+v", vars)
	}
	if vars["ref0"] != "refs/heads/main" {
		t.Fatalf("expected refs/heads/main, got %v", vars["ref0"])
	}
	if vars["ref1"] != "refs/heads/release/v4" {
		t.Fatalf("expected refs/heads/release/v4, got %v", vars["ref1"])
	}
}

func TestBatchReachabilitySize(t *testing.T) {
	if batchReachabilitySize != 50 {
		t.Fatalf("expected 50, got %d", batchReachabilitySize)
	}
}

// TestBatchBranchContains_IPAllowListRateLimitedFallback: the GraphQL denial is
// the only policy evidence when the repository metadata already answers, so it
// must be recorded for the rate-limited fallback to name it.
func TestBatchBranchContains_IPAllowListRateLimitedFallback(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(httpmock.REST("GET", `^/repos/acme/widget$`), httpmock.JSONResponse(map[string]any{"full_name": "acme/widget"}))
	reg.Register(httpmock.GraphQL(`compare\(headRef`), func(req *http.Request) (*http.Response, error) {
		resp, err := httpmock.JSONResponse(map[string]any{"message": ipAllowListDeniedMessage("acme")})(req)
		resp.StatusCode = http.StatusForbidden
		resp.Status = "403 Forbidden"
		return resp, err
	})
	reg.RegisterReusable(anyRequest, ipAllowListAPI("acme", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}

	// An earlier pin warms the metadata the fallback then relies on.
	_, err := c.RepoMetadata(context.Background(), "acme", "widget")
	require.NoError(t, err)

	_, checked, err := c.BatchBranchContains(context.Background(), "acme", "widget", paritySHA, []BranchHead{
		{Name: "main", SHA: parityMoved},
	})
	assert.False(t, checked)
	var rateLimited *SSORateLimitedError
	require.ErrorAs(t, err, &rateLimited)
	assert.Equal(t, ipAllowList, rateLimited.policy)
	assert.Contains(t, rateLimited.Error(), "IP allow list")
	assert.NotContains(t, rateLimited.Error(), "SAML")
}
