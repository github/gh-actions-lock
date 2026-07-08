package httpmock

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGraphQLAndRegistry(t *testing.T) {
	reg := &Registry{}
	reg.Register(
		GraphQL(`query Example\b`),
		JSONResponse(map[string]any{"data": map[string]any{"ok": true}}),
	)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", io.NopCloser(strings.NewReader(`{"query":"query Example { viewer { login } }"}`)))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	resp, err := reg.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	reg.Verify(t)
}

func TestGraphQLQueryResponder(t *testing.T) {
	reg := &Registry{}
	called := false
	reg.Register(
		GraphQL(`query Example\b`),
		GraphQLQuery(`{"data":{"viewer":{"login":"mona"}}}`, func(query string, variables map[string]any) {
			called = true
			if query == "" {
				t.Fatal("expected query to be passed to callback")
			}
		}),
	)

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", io.NopCloser(strings.NewReader(`{"query":"query Example { viewer { login } }","variables":{"x":1}}`)))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}

	_, err = reg.RoundTrip(req)
	if err != nil {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if !called {
		t.Fatal("expected GraphQLQuery callback to run")
	}
	reg.Verify(t)
}
