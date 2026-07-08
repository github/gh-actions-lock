package ghapi

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
)

func TestPeelTagObject_Tag(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQL("query"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"head":   map[string]any{"__typename": "Tag"},
					"peeled": map[string]any{"oid": "commitabc"},
				},
			},
		}),
	)

	c := newTestClient(t, reg)
	result, err := c.PeelTagObject(context.Background(), "actions", "checkout", "tagsha")
	if err != nil {
		t.Fatal(err)
	}
	if result.Typename != "Tag" {
		t.Fatalf("expected Tag, got %q", result.Typename)
	}
	if result.CommitOID != "commitabc" {
		t.Fatalf("expected commitabc, got %q", result.CommitOID)
	}
}

func TestPeelTagObject_Commit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQL("query"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"head":   map[string]any{"__typename": "Commit"},
					"peeled": nil,
				},
			},
		}),
	)

	c := newTestClient(t, reg)
	result, err := c.PeelTagObject(context.Background(), "actions", "checkout", "commitsha")
	if err != nil {
		t.Fatal(err)
	}
	if result.Typename != "Commit" {
		t.Fatalf("expected Commit, got %q", result.Typename)
	}
	if result.CommitOID != "" {
		t.Fatalf("expected empty CommitOID for non-tag, got %q", result.CommitOID)
	}
}

func TestPeelTagObject_NotFound(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQL("query"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"head":   nil,
					"peeled": nil,
				},
			},
		}),
	)

	c := newTestClient(t, reg)
	result, err := c.PeelTagObject(context.Background(), "actions", "checkout", "badsha")
	if err != nil {
		t.Fatal(err)
	}
	if result.Typename != "" || result.CommitOID != "" {
		t.Fatalf("expected zero result for not-found, got %+v", result)
	}
}

func TestPeelTagObject_NilRepo(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQL("query"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": nil,
			},
		}),
	)

	c := newTestClient(t, reg)
	result, err := c.PeelTagObject(context.Background(), "x", "y", "z")
	if err != nil {
		t.Fatal(err)
	}
	if result.Typename != "" {
		t.Fatalf("expected zero result for nil repo, got %+v", result)
	}
}

func newTestClient(t *testing.T, reg *httpmock.Registry) *Client {
	t.Helper()
	c, err := New("github.com", WithClientTransport(reg))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
