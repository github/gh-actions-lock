package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestNew_RESTOnlyForDependabotProxy(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", tt.value)
			c, err := New("github.com", WithClientTransport(roundTripFunc(nil)))
			if err != nil {
				t.Fatal(err)
			}
			if c.restOnly != tt.want {
				t.Fatalf("restOnly = %v, want %v", c.restOnly, tt.want)
			}
		})
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
		if req.Method == http.MethodGet {
			return jsonHTTP(map[string]any{"visibility": "public"})
		}
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
		if req.Method == http.MethodGet {
			return statusResponse(req, http.StatusUnauthorized)
		}
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

func TestResolveActionFiles_RESTOnlyUsesPrivateRepo(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("fallback request method = %s, want GET", r.Method)
		}
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			json.NewEncoder(w).Encode(map[string]string{"sha": "abc123def456abc123def456abc123def456abc1"})
		case strings.Contains(r.URL.Path, "/contents/"):
			fmt.Fprint(w, "name: public action")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("REST-only mode used authenticated transport: %s %s", req.Method, req.URL)
		return nil, nil
	})
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	c.anonBaseURL = srv.URL

	results := c.ResolveActionFiles(context.Background(), []ActionFileRequest{
		{Owner: "actions", Repo: "checkout", Ref: "v4"},
		{Owner: "actions", Repo: "setup-go", Path: "cache", Ref: "v5"},
	})
	for _, result := range results {
		nwo := result.Owner + "/" + result.Repo
		if result.Err != nil {
			t.Fatalf("%s: %v", nwo, result.Err)
		}
		if result.CommitOID != "abc123def456abc123def456abc123def456abc1" {
			t.Fatalf("%s: commit = %q", nwo, result.CommitOID)
		}
		if result.ActionYML != "name: public action" {
			t.Fatalf("%s: action.yml = %q", nwo, result.ActionYML)
		}
	}
}

func TestResolveActionFiles_BadCredentialsFallbackFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		repoStatus int
	}{
		{name: "inaccessible repository", repoStatus: http.StatusNotFound},
		{name: "unreachable ref", repoStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fallbackCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls++
				http.NotFound(w, r)
			}))
			defer srv.Close()

			tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if tt.repoStatus != http.StatusOK {
						return statusResponse(req, tt.repoStatus)
					}
					return jsonHTTP(map[string]any{"visibility": "private"})
				}
				return badCredentialsResponse(req)
			})
			c, err := New("github.com", WithClientTransport(tr))
			if err != nil {
				t.Fatal(err)
			}
			c.anonBaseURL = srv.URL

			results := c.ResolveActionFiles(context.Background(), []ActionFileRequest{
				{Owner: "example", Repo: "action", Ref: "v1"},
			})
			if results[0].Err == nil {
				t.Fatal("expected resolution error")
			}
			if tt.repoStatus != http.StatusOK && fallbackCalls != 0 {
				t.Fatalf("inaccessible repository made %d fallback requests", fallbackCalls)
			}
			if tt.repoStatus == http.StatusOK && fallbackCalls == 0 {
				t.Fatal("unreachable ref was not attempted over REST")
			}
		})
	}
}

func TestPeelTagObject_RESTOnlyUsesRESTFallback(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("fallback request method = %s, want GET", r.Method)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/tagsha"):
			json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]string{"type": "tag", "sha": "innertag"},
			})
		case strings.HasSuffix(r.URL.Path, "/innertag"):
			json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]string{"type": "commit", "sha": "commitabc"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("REST-only mode used authenticated transport: %s %s", req.Method, req.URL)
		return nil, nil
	})
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	c.anonBaseURL = srv.URL

	result, err := c.PeelTagObject(context.Background(), "actions", "checkout", "tagsha")
	if err != nil {
		t.Fatal(err)
	}
	if result.Typename != "Tag" || result.CommitOID != "commitabc" {
		t.Fatalf("unexpected peel result: %+v", result)
	}
}

func TestBatchBranchContains_RESTOnlyUsesRESTFallback(t *testing.T) {
	t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("fallback request method = %s, want GET", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"merge_base_commit": map[string]string{"sha": "targetsha"},
		})
	}))
	defer srv.Close()

	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("REST-only mode used authenticated transport: %s %s", req.Method, req.URL)
		return nil, nil
	})
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	c.anonBaseURL = srv.URL

	matched, checked, err := c.BatchBranchContains(context.Background(), "actions", "checkout", "targetsha", []BranchHead{
		{Name: "main", SHA: "headsha"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !checked || matched != "main" {
		t.Fatalf("matched = %q, checked = %v", matched, checked)
	}
}

func badCredentialsResponse(req *http.Request) (*http.Response, error) {
	return statusResponse(req, http.StatusUnauthorized)
}

func statusResponse(req *http.Request, status int) (*http.Response, error) {
	resp, err := jsonHTTP(map[string]string{"message": http.StatusText(status)})
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Request = req
	return resp, err
}
