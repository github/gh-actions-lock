package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		case strings.Count(r.URL.Path, "/") == 3:
			json.NewEncoder(w).Encode(map[string]string{"full_name": strings.TrimPrefix(r.URL.Path, "/repos/")})
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

func TestSSOFallbackEligible_CachesOnlyDefinitiveAnswers(t *testing.T) {
	rateLimited := func(w http.ResponseWriter) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}
	tests := []struct {
		name       string
		first      func(http.ResponseWriter)
		wantProbes int
		wantSecond bool
	}{
		{"rate limit is retried", rateLimited, 2, true},
		{"429 is retried", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }, 2, true},
		{"5xx is retried", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, 2, true},
		{"404 is cached", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anonProbeCache = sync.Map{}
			anonRateLimited = sync.Map{}
			probes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes++
				if probes == 1 {
					tt.first(w)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			c := &Client{Hostname: "github.com", anonBaseURL: srv.URL}

			if c.SSOFallbackEligible(context.Background(), "acme") {
				t.Fatal("first probe: want ineligible")
			}
			if got := c.SSOFallbackEligible(context.Background(), "acme"); got != tt.wantSecond {
				t.Errorf("second probe eligible = %v, want %v", got, tt.wantSecond)
			}
			if probes != tt.wantProbes {
				t.Errorf("probes = %d, want %d", probes, tt.wantProbes)
			}
		})
	}
}

// TestSSOFallbackEligible_ClearsStaleRateLimit: a later definitive or 5xx
// answer must drop the rate-limit marker so errors stop blaming it.
func TestSSOFallbackEligible_ClearsStaleRateLimit(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			anonProbeCache = sync.Map{}
			anonRateLimited = sync.Map{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			c := &Client{Hostname: "github.com", anonBaseURL: srv.URL}
			anonRateLimited.Store(c.anonBase()+"/acme", struct{}{})

			c.SSOFallbackEligible(context.Background(), "acme")
			if _, ok := anonRateLimited.Load(c.anonBase() + "/acme"); ok {
				t.Error("stale rate-limit marker survived a later response")
			}
		})
	}
}

func TestSSOFallback_RateLimitedGuidance(t *testing.T) {
	tests := []struct {
		name string
		anon func(w http.ResponseWriter, r *http.Request)
	}{
		{"probe rate-limited", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		}},
		{"anonymous request rate-limited", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anonProbeCache = sync.Map{}
			anonRateLimited = sync.Map{}
			srv := httptest.NewServer(http.HandlerFunc(tt.anon))
			defer srv.Close()
			saml := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, _ := jsonHTTP(map[string]any{"message": "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."})
				resp.StatusCode = http.StatusForbidden
				resp.Request = req
				return resp, nil
			})
			c, err := New("github.com", WithClientTransport(saml))
			if err != nil {
				t.Fatal(err)
			}
			c.anonBaseURL = srv.URL
			c.anonHTTP = srv.Client()

			_, err = c.CommitSHA(context.Background(), "acme", "widget", "v1")
			var rl *SSORateLimitedError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want SSORateLimitedError", err)
			}
			for _, want := range []string{
				`SSO authorization required: your token is not authorized for the "acme" organization`,
				"https://github.com/orgs/acme/sso",
				"anonymous fallback for public repositories was rate-limited",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

// TestCompareRefs_SSOFallback: a recorded ref that moved in a public
// repository behind an org's SSO is still compared anonymously.
func TestCompareRefs_SSOFallback(t *testing.T) {
	anonProbeCache = sync.Map{}
	anonRateLimited = sync.Map{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/acme":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/repos/acme/widget/compare/"):
			json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "merge_base_commit": map[string]string{"sha": "aaa"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	saml := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, _ := jsonHTTP(map[string]any{"message": "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."})
		resp.StatusCode = http.StatusForbidden
		resp.Request = req
		return resp, nil
	})
	c, err := New("github.com", WithClientTransport(saml))
	if err != nil {
		t.Fatal(err)
	}
	c.anonBaseURL = srv.URL
	c.anonHTTP = srv.Client()

	status, base, err := c.CompareRefs(context.Background(), "acme", "widget", "aaa", "bbb")
	if err != nil {
		t.Fatalf("expected anonymous fallback to succeed, got %v", err)
	}
	if status != "ahead" || base != "aaa" {
		t.Fatalf("got status=%q base=%q", status, base)
	}
}

func resetAnonFallbackState() {
	anonProbeCache = sync.Map{}
	anonRateLimited = sync.Map{}
	anonIPBlocked = sync.Map{}
}

func ipAllowListDeniedMessage(owner string) string {
	return "Although you appear to have the correct authorization credentials, the `" + owner + "` organization has an IP allow list enabled, and your IP address is not permitted to access this resource."
}

func anyRequest(*http.Request) bool { return true }

// ipAllowListAPI answers token-bearing requests with the organization IP
// allow list 403 and hands anonymous ones to anon.
func ipAllowListAPI(owner string, anon http.HandlerFunc) httpmock.Responder {
	return func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			rec := httptest.NewRecorder()
			anon(rec, req)
			return rec.Result(), nil
		}
		resp, err := httpmock.JSONResponse(map[string]any{"message": ipAllowListDeniedMessage(owner)})(req)
		resp.StatusCode = http.StatusForbidden
		resp.Status = "403 Forbidden"
		return resp, err
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestIsIPAllowListDenial(t *testing.T) {
	message := ipAllowListDeniedMessage("acme")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"HTTP 403", &api.HTTPError{StatusCode: http.StatusForbidden, Message: message}, true},
		{"GraphQL forbidden", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "FORBIDDEN", Message: message}}}, true},
		{"bare message", errors.New(message), false},
		{"ordinary forbidden", &api.HTTPError{StatusCode: http.StatusForbidden, Message: "Resource not accessible by integration"}, false},
		{"wrong GraphQL item", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "NOT_FOUND", Message: message}, {Type: "FORBIDDEN", Message: "ordinary denial"}}}, false},
		{"GraphQL rate limit", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "FORBIDDEN", Message: message}, {Type: "RATE_LIMITED"}}}, false},
		{"HTTP rate limit", &api.HTTPError{StatusCode: http.StatusForbidden, Message: message, Headers: http.Header{"X-Ratelimit-Remaining": {"0"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isIPAllowListDenial(tt.err))
		})
	}
}

func TestIPAllowListRateLimitedGuidance(t *testing.T) {
	ctx := context.Background()
	pins := func(c *Client) error {
		return c.CheckPins(ctx, []PinCheck{{Owner: "acme", Repo: "widget", SHA: paritySHA}})[0].Err
	}
	ancestry := func(c *Client) error {
		_, _, err := c.BatchBranchContains(ctx, "acme", "widget", paritySHA, []BranchHead{{Name: "main", SHA: parityMoved}})
		return err
	}
	action := func(c *Client) error {
		return c.ResolveActionFiles(ctx, []ActionFileRequest{{Owner: "acme", Repo: "widget", Ref: "v1"}})[0].Err
	}
	tests := []struct {
		name, limitedPath  string
		check              func(*Client) error
		wantRateLimitCause bool
	}{
		{"REST commit probe", "/orgs/acme", func(c *Client) error {
			_, err := c.CommitSHA(ctx, "acme", "widget", "v1")
			return err
		}, false},
		{"pin probe", "/orgs/acme", pins, false},
		{"pin metadata", "/repos/acme/widget", pins, false},
		{"ancestry probe", "/orgs/acme", ancestry, false},
		{"ancestry metadata", "/repos/acme/widget", ancestry, false},
		{"action commit", "/repos/acme/widget/commits/v1", action, true},
		{"action contents", "/repos/acme/widget/contents/action.yml", action, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetAnonFallbackState()
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.RegisterReusable(anyRequest, ipAllowListAPI("acme", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tt.limitedPath {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				switch r.URL.Path {
				case "/orgs/acme":
					w.WriteHeader(http.StatusOK)
				case "/repos/acme/widget":
					writeJSON(w, map[string]any{"full_name": "acme/widget"})
				case "/repos/acme/widget/commits/v1":
					writeJSON(w, map[string]any{"sha": paritySHA})
				default:
					t.Errorf("unexpected anonymous request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			c := newTestClient(t, reg)
			c.anonHTTP = &http.Client{Transport: reg}

			err := tt.check(c)
			var limited *SSORateLimitedError
			require.ErrorAs(t, err, &limited)
			assert.True(t, IPAllowListBlocked(err))
			assert.Contains(t, Brief(err), "IP allow list")
			assert.Contains(t, Brief(err), "anonymous fallback for public repositories was rate-limited")
			assert.NotContains(t, Brief(err), "SSO")
			if tt.wantRateLimitCause {
				assert.True(t, IsRateLimited(err), "the anonymous rate-limit cause must survive")
			}
		})
	}
}
