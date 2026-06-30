package ghapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSSOFallbackEligible(t *testing.T) {
	// Stand up a server that returns 200 for /orgs/actions and 404 for others.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/actions":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{Hostname: "github.com", anonBaseURL: srv.URL}
	ctx := context.Background()

	// Reset the global cache between subtests.
	anonProbeCache = sync.Map{}

	if !c.SSOFallbackEligible(ctx, "actions") {
		t.Error("expected actions org to be eligible")
	}
	if c.SSOFallbackEligible(ctx, "myorg") {
		t.Error("expected myorg to NOT be eligible")
	}
}

func TestResolveActionFiles_SSOFallbackForActionsOrg(t *testing.T) {
	anonProbeCache = sync.Map{} // reset cache

	// Stand up a fake API server for anonymous resolution.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/actions":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/commits/"):
			json.NewEncoder(w).Encode(map[string]string{"sha": "abc123def456abc123def456abc123def456abc1"})
		case strings.Contains(r.URL.Path, "/contents/"):
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("name: checkout\ndescription: Checkout action"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// GraphQL transport returns SAML error for actions/checkout.
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonHTTP(map[string]any{
			"data": map[string]any{"a0": nil},
			"errors": []map[string]any{
				{
					"message":    "Resource protected by organization SAML enforcement.",
					"path":       []any{"a0"},
					"extensions": map[string]any{"saml_failure": true},
				},
			},
		})
	})

	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	// Point anonymous calls at our test server instead of api.github.com.
	c.Hostname = strings.TrimPrefix(srv.URL, "http://")
	// Patch resolveAnonymous to use http:// scheme via a test override.
	c.anonBaseURL = srv.URL

	refs := []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v4"},
	}

	results := c.ResolveActionFiles(context.Background(), refs)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("expected anonymous fallback to succeed, got: %v", results[0].Err)
	}
	if results[0].CommitOID != "abc123def456abc123def456abc123def456abc1" {
		t.Fatalf("expected resolved SHA, got %q", results[0].CommitOID)
	}
	if results[0].ActionYML != "name: checkout\ndescription: Checkout action" {
		t.Fatalf("expected action.yml content, got %q", results[0].ActionYML)
	}
}

func TestResolveActionFiles_SSONoFallbackForNonActionsOrg(t *testing.T) {
	anonProbeCache = sync.Map{} // reset cache

	// Probe server: returns 404 for /orgs/mycompany (private org).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonHTTP(map[string]any{
			"data": map[string]any{"a0": nil},
			"errors": []map[string]any{
				{
					"message":    "Resource protected by organization SAML enforcement.",
					"path":       []any{"a0"},
					"extensions": map[string]any{"saml_failure": true},
				},
			},
		})
	})

	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	c.anonBaseURL = srv.URL

	refs := []ActionFileRequest{
		{Owner: "mycompany", Repo: "private-action", Ref: "v1"},
	}

	results := c.ResolveActionFiles(context.Background(), refs)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("expected SSO error for non-actions org")
	}
	if !strings.Contains(results[0].Err.Error(), "SSO authorization required") {
		t.Fatalf("expected SSO error, got: %v", results[0].Err)
	}
}
