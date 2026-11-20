package ghapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ipAllowListClient returns a client whose token every endpoint refuses with
// acme's IP allow list denial. Anonymous requests, which go to anon, are
// recorded in paths.
func ipAllowListClient(t *testing.T, owner string, anon http.HandlerFunc) (c *Client, paths *[]string) {
	t.Helper()
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	paths = new([]string)
	reg.RegisterReusable(anyRequest, ipAllowListAPI(owner, func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.Method+" "+r.URL.Path)
		anon(w, r)
	}))
	c = newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}
	return c, paths
}

// An outage or rate limit in the fallback's own metadata read says nothing
// about the repository, so it must not be reported as the final IP denial.
func TestIPAllowListFallback_MetadataFailure(t *testing.T) {
	ctx := context.Background()
	checks := map[string]func(*Client) error{
		"pins": func(c *Client) error {
			return c.CheckPins(ctx, []PinCheck{{Owner: "acme", Repo: "widget", SHA: paritySHA}})[0].Err
		},
		"action files": func(c *Client) error {
			return c.ResolveActionFiles(ctx, []ActionFileRequest{{Owner: "acme", Repo: "widget", Ref: "v1"}})[0].Err
		},
		"ancestry": func(c *Client) error {
			_, _, err := c.BatchBranchContains(ctx, "acme", "widget", paritySHA, []BranchHead{{Name: "main", SHA: parityMoved}})
			return err
		},
		"peel": func(c *Client) error {
			_, err := c.PeelTagObject(ctx, "acme", "widget", paritySHA)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			for _, tt := range []struct {
				name       string
				status     int
				wantRetry  bool
				wantDenial bool // the error still names the IP allow list
			}{
				{"outage", http.StatusServiceUnavailable, true, false},
				{"rate limit", http.StatusTooManyRequests, true, true}, // still names the policy
				{"repository not readable", http.StatusNotFound, false, true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/orgs/acme" {
							w.WriteHeader(http.StatusOK)
							return
						}
						w.WriteHeader(tt.status)
					})
					err := check(c)
					require.Error(t, err)
					assert.Equal(t, tt.wantRetry, IsTransient(err), "err = %v", err)
					assert.Equal(t, tt.wantDenial, IPAllowListBlocked(err), "err = %v", err)
				})
			}
		})
	}
}

// A probe that hit an outage is not an answer about the owner either.
func TestIPAllowListFallback_ProbeOutage(t *testing.T) {
	c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c.CommitSHA(context.Background(), "acme", "widget", "v1")
	require.Error(t, err)
	assert.True(t, IsTransient(err), "err = %v", err)
	code, _ := StatusCode(err)
	assert.Equal(t, http.StatusBadGateway, code)
}

// The rate-limited probe's reset time must reach callers, which decide on it
// whether a check is unanswered.
func TestIPAllowListFallback_RateLimitKeepsResetTime(t *testing.T) {
	c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, _, err := c.CompareRefs(context.Background(), "acme", "widget", paritySHA, parityMoved)
	require.Error(t, err)
	assert.True(t, IPAllowListBlocked(err))
	assert.True(t, IsRateLimited(err))
	assert.False(t, RetryAt(err).IsZero(), "reset time was dropped")
}

// SAML blocks keep their own error: authorizing the token is the fix, so a
// rate-limited fallback is not an outage there.
func TestSAMLFallbackRateLimit_KeepsDenial(t *testing.T) {
	resetAnonFallbackState()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{Hostname: "github.com", anonBaseURL: srv.URL}
	require.False(t, c.SSOFallbackEligible(context.Background(), "acme"))

	saml := &api.HTTPError{StatusCode: http.StatusForbidden, Message: "Resource protected by organization SAML enforcement."}
	err := c.ssoErr("acme", saml)
	var limited *SSORateLimitedError
	require.ErrorAs(t, err, &limited)
	assert.Same(t, saml, limited.Err)
	assert.False(t, IsRateLimited(err))
	assert.False(t, IPAllowListBlocked(err))
}

func TestCompareCommits_AnonymousFallbackUnprocessable(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			compares := 0
			c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/orgs/acme" {
					w.WriteHeader(http.StatusOK)
					return
				}
				compares++
				w.WriteHeader(status)
			})
			for range 2 {
				ok, err := c.CompareCommits(context.Background(), "acme", "widget", paritySHA, parityMoved)
				require.NoError(t, err)
				assert.False(t, ok)
			}
			assert.Equal(t, 1, compares, "the false verdict is cached")
		})
	}
}

// A public repository transferred from a user into an IP-restricted
// organization still answers under its old name, which names no organization.
func TestIPAllowListFallback_TransferredRepository(t *testing.T) {
	anon := func(repoStatus int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/orgs/alice":
				w.WriteHeader(http.StatusNotFound)
			case "/repos/alice/action":
				if repoStatus != http.StatusOK {
					w.WriteHeader(repoStatus)
					return
				}
				writeJSON(w, map[string]any{"full_name": "acme/action"})
			case "/repos/alice/action/commits/v1":
				writeJSON(w, map[string]any{"sha": paritySHA})
			default:
				http.NotFound(w, r)
			}
		}
	}

	t.Run("public", func(t *testing.T) {
		c, _ := ipAllowListClient(t, "acme", anon(http.StatusOK))
		sha, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
		require.NoError(t, err)
		assert.Equal(t, paritySHA, sha)
	})
	t.Run("not public", func(t *testing.T) {
		c, paths := ipAllowListClient(t, "acme", anon(http.StatusNotFound))
		_, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
		require.Error(t, err)
		assert.True(t, IPAllowListBlocked(err), "err = %v", err)
		assert.NotContains(t, *paths, "GET /repos/alice/action/commits/v1")
	})
	t.Run("rate limited probe is retried, not cached", func(t *testing.T) {
		c, _ := ipAllowListClient(t, "acme", anon(http.StatusTooManyRequests))
		_, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
		require.Error(t, err)
		assert.True(t, IsRateLimited(err), "err = %v", err)
		_, cached := anonProbeCache.Load(c.anonBase() + "/alice/action")
		assert.False(t, cached)
	})
	t.Run("SAML does not probe the repository", func(t *testing.T) {
		resetAnonFallbackState()
		reg := &httpmock.Registry{}
		var paths []string
		reg.RegisterReusable(anyRequest, func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") == "" {
				paths = append(paths, req.URL.Path)
				return httpmock.StatusResponse(http.StatusNotFound)(req)
			}
			resp, err := httpmock.JSONResponse(map[string]any{"message": "Resource protected by organization SAML enforcement."})(req)
			resp.StatusCode = http.StatusForbidden
			return resp, err
		})
		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		_, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
		require.Error(t, err)
		assert.Equal(t, []string{"/orgs/alice"}, paths)
	})
}

// A github.com public client that carries a token retries an IP-policy denial
// without it; the tenant host's own client never falls back to github.com.
func TestPublicClient_IPAllowListRetriesWithoutCredentials(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.RegisterReusable(hostRequest("api.github.com", `repos/o/r$`), func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			resp, err := httpmock.JSONResponse(map[string]any{"message": ipAllowListDeniedMessage("acme")})(req)
			resp.StatusCode = http.StatusForbidden
			return resp, err
		}
		return httpmock.JSONResponse(map[string]any{"visibility": "public", "id": 2, "owner": map[string]any{"id": 1}})(req)
	})
	c, err := New("tenant.ghe.com", WithClientTransport(reg), withStoredLogin(t, "dotcom-secret"))
	require.NoError(t, err)

	selected, err := c.ForRepo(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, "github.com", selected.Hostname)

	var authenticated, anonymous int
	for _, req := range reg.Requests {
		if req.URL.Host != "api.github.com" {
			continue
		}
		if req.Header.Get("Authorization") == "" {
			anonymous++
		} else {
			authenticated++
		}
	}
	assert.Equal(t, 1, authenticated)
	assert.Equal(t, 1, anonymous)
}

func TestPublicClient_NoRetryForOrdinaryDenial(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(hostRequest("api.tenant.ghe.com", `repos/o/r$`), httpmock.StatusResponse(http.StatusNotFound))
	reg.RegisterReusable(hostRequest("api.github.com", `repos/o/r$`), func(req *http.Request) (*http.Response, error) {
		resp, err := httpmock.JSONResponse(map[string]any{"message": "Resource not accessible"})(req)
		resp.StatusCode = http.StatusForbidden
		return resp, err
	})
	c, err := New("tenant.ghe.com", WithClientTransport(reg), withStoredLogin(t, "dotcom-secret"))
	require.NoError(t, err)

	_, err = c.ForRepo(context.Background(), "o", "r")
	require.Error(t, err)
	var githubRequests int
	for _, req := range reg.Requests {
		if req.URL.Host == "api.github.com" {
			githubRequests++
		}
	}
	assert.Equal(t, 1, githubRequests, "only an IP allow list denial is retried")
}
