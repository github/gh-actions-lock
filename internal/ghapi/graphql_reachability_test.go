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

// Cached metadata must not erase the policy that forced a rate-limited fallback.
func TestBatchBranchContains_IPAllowListRateLimitedFallback(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(httpmock.REST("GET", `^/repos/acme/widget$`), httpmock.JSONResponse(map[string]any{"full_name": "acme/widget"}))
	reg.Register(httpmock.GraphQL(`compare\(headRef`), httpmock.JSONResponse(map[string]any{
		"data":   map[string]any{"repo": nil},
		"errors": []map[string]any{{"type": "FORBIDDEN", "message": ipAllowListDeniedMessage("acme"), "path": []any{"repo"}}},
	}))
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
	assert.Contains(t, rateLimited.Error(), "IP allow list")
	assert.NotContains(t, rateLimited.Error(), "SAML")
}
