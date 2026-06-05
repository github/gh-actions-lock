package doctor

import (
	"context"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/httpmock"
)

func newTagListerWithRegistry(t *testing.T, reg *httpmock.Registry) *TagLister {
	t.Helper()
	client, err := api.NewRESTClient(api.ClientOptions{
		Host:      "github.com",
		AuthToken: "test",
		Transport: reg,
	})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}
	return NewTagLister(client)
}

// TestMatchesSHA verifies a pin matches both the peeled commit SHA and the
// annotated tag-object SHA (what immutable releases pin to).
func TestMatchesSHA(t *testing.T) {
	tag := TagInfo{Name: "v9.0.0", SHA: "3a2844b", TagObjectSHA: "d746ffe"}

	if !tag.MatchesSHA("3a2844b") {
		t.Error("expected match on peeled commit SHA")
	}
	if !tag.MatchesSHA("d746ffe") {
		t.Error("expected match on tag-object SHA")
	}
	if tag.MatchesSHA("deadbee") {
		t.Error("unexpected match on unrelated SHA")
	}
	if tag.MatchesSHA("") {
		t.Error("empty SHA should never match")
	}

	// Lightweight tag (no tag object): only the commit SHA matches.
	light := TagInfo{Name: "v1", SHA: "abc123"}
	if !light.MatchesSHA("abc123") {
		t.Error("expected lightweight tag to match its commit SHA")
	}
	if light.MatchesSHA("d746ffe") {
		t.Error("lightweight tag should not match an unrelated SHA")
	}
}

// TestSuggestTagsForSHA_ImmutableTagObject verifies that a SHA which is the
// annotated tag-object SHA (not the peeled commit) is recognized as the
// release tag rather than treated as an unreleased commit.
func TestSuggestTagsForSHA_ImmutableTagObject(t *testing.T) {
	reg := &httpmock.Registry{}
	// repos/.../tags dereferences annotated tags to their commit SHA.
	reg.Register(
		httpmock.REST("GET", `repos/actions/github-script/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v9.0.0", "commit": map[string]any{"sha": "3a2844b7e9c422d3c10d287c895573f7108da1b3"}},
		}),
	)
	// git/matching-refs/tags exposes the raw object SHA (the tag object for
	// annotated tags). This is what immutable-release pins target.
	reg.Register(
		httpmock.REST("GET", `repos/actions/github-script/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"ref": "refs/tags/v9.0.0", "object": map[string]any{
				"sha":  "d746ffe35508b1917358783b479e04febd2b8f71",
				"type": "tag",
			}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/github-script/releases`),
		httpmock.JSONResponse([]map[string]any{}),
	)

	tl := newTagListerWithRegistry(t, reg)

	// Pin to the tag-object SHA, as immutable releases do.
	suggestions, err := tl.SuggestTagsForSHA(context.Background(), "actions", "github-script", "d746ffe35508b1917358783b479e04febd2b8f71")
	if err != nil {
		t.Fatalf("SuggestTagsForSHA: %v", err)
	}
	if len(suggestions) == 0 {
		t.Fatal("expected the tag-object SHA to be recognized as v9.0.0, got no suggestions")
	}
	if suggestions[0].Tag.Name != "v9.0.0" {
		t.Fatalf("expected v9.0.0, got %q", suggestions[0].Tag.Name)
	}
}
