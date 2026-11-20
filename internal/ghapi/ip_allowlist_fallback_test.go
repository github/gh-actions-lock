package ghapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

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
	reason, limited := Outage(err)
	assert.True(t, limited)
	assert.Equal(t, "anonymous GitHub API rate limit exhausted (an IP allow list refused the token)", reason)
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
	_, err := c.orgPolicyFallback(context.Background(), "acme", "widget", saml)
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
				switch r.URL.Path {
				case "/orgs/acme":
					w.WriteHeader(http.StatusOK)
				case "/repos/acme/widget":
					writeJSON(w, map[string]any{"full_name": "acme/widget"})
				default:
					compares++
					w.WriteHeader(status)
				}
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

// Anonymous reads can't see a private repository, so an anonymous 404 behind
// an IP allow list is not a verdict on the ref, the commit or the history:
// the denial stands and nothing is cached as "not an ancestor".
func TestIPAllowListFallback_PrivateRepository(t *testing.T) {
	ctx := context.Background()
	reads := map[string]func(*Client) error{
		"commit": func(c *Client) error { _, err := c.CommitSHA(ctx, "acme", "widget", "v1"); return err },
		"releases": func(c *Client) error {
			_, err := c.Releases(ctx, "acme", "widget")
			return err
		},
		"branches": func(c *Client) error { _, err := c.ListBranches(ctx, "acme", "widget"); return err },
		"tags":     func(c *Client) error { _, err := c.ListTags(ctx, "acme", "widget"); return err },
		"all tags": func(c *Client) error { _, err := c.TagsAtCommit(ctx, "acme", "widget", paritySHA); return err },
		"compare": func(c *Client) error {
			_, err := c.CompareCommits(ctx, "acme", "widget", paritySHA, parityMoved)
			return err
		},
		"compare refs": func(c *Client) error {
			_, _, err := c.CompareRefs(ctx, "acme", "widget", paritySHA, parityMoved)
			return err
		},
		"git ref":  func(c *Client) error { _, _, err := c.fetchGitRef(ctx, "acme", "widget", "refs/tags/v1"); return err },
		"metadata": func(c *Client) error { _, err := c.repoMetadata(ctx, "acme", "widget"); return err },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/orgs/acme" {
					w.WriteHeader(http.StatusOK)
					return
				}
				http.NotFound(w, r)
			})
			err := read(c)
			require.Error(t, err)
			assert.True(t, IPAllowListBlocked(err), "err = %v", err)
			assert.False(t, IsNotFound(err), "err = %v", err)
		})
	}
}

// For a repository the anonymous read can see, a 404 or 422 is still a
// verdict on the ref or the commits, as it is for an authenticated read.
func TestIPAllowListFallback_PublicRepositoryMissingRef(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/orgs/acme":
					w.WriteHeader(http.StatusOK)
				case "/repos/acme/widget":
					writeJSON(w, map[string]any{"full_name": "acme/widget"})
				default:
					w.WriteHeader(status)
				}
			})
			_, found, err := c.restCommit(ctx, "acme", "widget", "v1")
			require.NoError(t, err)
			assert.False(t, found)

			_, _, err = c.fetchGitRef(ctx, "acme", "widget", "refs/tags/v1")
			require.NoError(t, err)

			_, _, err = c.CompareRefs(ctx, "acme", "widget", paritySHA, parityMoved)
			code, ok := StatusCode(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, status, code)
			assert.False(t, IPAllowListBlocked(err))

			_, err = c.CommitSHA(ctx, "acme", "widget", "v1")
			code, ok = StatusCode(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, status, code)
		})
	}
}

// GraphQL may be refused while an authenticated REST read of the repository
// succeeds, so that metadata proves nothing about what an anonymous read sees:
// only a repository the credential-free read can see takes the fallback.
func TestIPAllowListFallback_AuthenticatedMetadataIsNotProof(t *testing.T) {
	ctx := context.Background()
	graphqlOnly := func(t *testing.T, anonRepo http.HandlerFunc) (*Client, *[]string) {
		t.Helper()
		resetAnonFallbackState()
		reg := &httpmock.Registry{}
		var anon []string
		reg.RegisterReusable(anyRequest, func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "" {
				if req.URL.Path == "/graphql" {
					resp, err := httpmock.JSONResponse(map[string]any{"message": ipAllowListDeniedMessage("acme")})(req)
					resp.StatusCode = http.StatusForbidden
					return resp, err
				}
				return httpmock.JSONResponse(map[string]any{"full_name": "acme/widget", "id": 2, "owner": map[string]any{"id": 1}})(req)
			}
			rec := httptest.NewRecorder()
			anon = append(anon, req.URL.Path)
			if req.URL.Path == "/orgs/acme" {
				rec.WriteHeader(http.StatusOK)
			} else {
				anonRepo(rec, req)
			}
			resp := rec.Result()
			resp.Request = req
			return resp, nil
		})
		c := newTestClient(t, reg)
		c.anonHTTP = &http.Client{Transport: reg}
		return c, &anon
	}
	checks := map[string]func(*Client) error{
		"pins": func(c *Client) error {
			return c.CheckPins(ctx, []PinCheck{{Owner: "acme", Repo: "widget", SHA: paritySHA}})[0].Err
		},
		"action files": func(c *Client) error {
			return c.ResolveActionFiles(ctx, []ActionFileRequest{{Owner: "acme", Repo: "widget", Ref: "v1"}})[0].Err
		},
	}
	for name, check := range checks {
		t.Run(name+"/not public", func(t *testing.T) {
			c, anon := graphqlOnly(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
			err := check(c)
			require.Error(t, err)
			assert.True(t, IPAllowListBlocked(err), "err = %v", err)
			assert.False(t, IsNotFound(err), "err = %v", err)
			assert.NotContains(t, *anon, "/repos/acme/widget/commits/v1", "no anonymous read of a repository it can't see")
		})
		t.Run(name+"/outage", func(t *testing.T) {
			c, _ := graphqlOnly(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
			err := check(c)
			require.Error(t, err)
			assert.True(t, IsTransient(err), "err = %v", err)
		})
	}
}

// The same reads succeed for a public repository.
func TestIPAllowListFallback_PublicRepositoryReads(t *testing.T) {
	ctx := context.Background()
	c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/acme":
			w.WriteHeader(http.StatusOK)
		case "/repos/acme/widget":
			writeJSON(w, map[string]any{"full_name": "acme/widget", "default_branch": "main", "id": 2, "owner": map[string]any{"id": 1}})
		case "/repos/acme/widget/releases":
			writeJSON(w, []map[string]any{{"tag_name": "v1"}})
		case "/repos/acme/widget/branches":
			writeJSON(w, []map[string]any{{"name": "main", "commit": map[string]string{"sha": paritySHA}}})
		case "/repos/acme/widget/tags":
			writeJSON(w, []map[string]any{{"name": "v1", "commit": map[string]string{"sha": paritySHA}}})
		default:
			t.Errorf("unexpected anonymous request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	releases, err := c.Releases(ctx, "acme", "widget")
	require.NoError(t, err)
	require.Len(t, releases, 1)
	branches, err := c.ListBranches(ctx, "acme", "widget")
	require.NoError(t, err)
	require.Len(t, branches, 1)
	tags, err := c.ListTags(ctx, "acme", "widget")
	require.NoError(t, err)
	require.Len(t, tags, 1)
	assert.Equal(t, "main", c.GetDefaultBranch(ctx, "acme", "widget"))
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

// A repository probe or an organization probe that ends in an outage is not
// an answer: the IP denial must not be reported as final.
func TestIPAllowListFallback_ProbeFailures(t *testing.T) {
	t.Run("repository probe outage", func(t *testing.T) {
		c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/orgs/alice" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
		require.Error(t, err)
		assert.True(t, IsTransient(err), "err = %v", err)
		code, _ := StatusCode(err)
		assert.Equal(t, http.StatusServiceUnavailable, code)
	})
	t.Run("organization probe transport failure", func(t *testing.T) {
		c, _ := ipAllowListClient(t, "acme", func(http.ResponseWriter, *http.Request) {})
		c.anonHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		})}
		_, err := c.CommitSHA(context.Background(), "acme", "widget", "v1")
		require.Error(t, err)
		assert.True(t, IsTransient(err), "err = %v", err)
		assert.False(t, IPAllowListBlocked(err))
	})
	t.Run("canceled context is not an outage", func(t *testing.T) {
		c, _ := ipAllowListClient(t, "acme", func(http.ResponseWriter, *http.Request) {})
		ctx, cancel := context.WithCancel(context.Background())
		c.anonHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			cancel()
			return nil, ctx.Err()
		})}
		_, err := c.CommitSHA(ctx, "acme", "widget", "v1")
		require.Error(t, err)
		assert.False(t, IsTransient(err), "err = %v", err)
	})
}

// publicIPAllowListClient is the github.com public client of a *.ghe.com run
// with a stored token, which an IP allow list refuses: every request carrying
// it gets the denial, and anon answers the rest. Requests are recorded.
func publicIPAllowListClient(t *testing.T, anon http.HandlerFunc) (c *Client, requests *[]*http.Request) {
	t.Helper()
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	reg.RegisterReusable(anyRequest, func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			resp, err := httpmock.JSONResponse(map[string]any{"message": ipAllowListDeniedMessage("acme")})(req)
			resp.StatusCode = http.StatusForbidden
			return resp, err
		}
		rec := httptest.NewRecorder()
		anon(rec, req)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})
	root, err := New("tenant.ghe.com", WithClientTransport(reg), withStoredLogin(t, "dotcom-secret"))
	require.NoError(t, err)
	require.NotNil(t, root.public)
	return root.public, &reg.Requests
}

// The public client's own REST reads and its eligibility probe carry a token
// the IP allow list refuses; both must fall back without credentials.
func TestPublicClient_IPAllowListFallbackWithoutCredentials(t *testing.T) {
	c, requests := publicIPAllowListClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/orgs/acme":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/acme/widget/commits/v1":
			writeJSON(w, map[string]any{"sha": paritySHA})
		default:
			http.NotFound(w, r)
		}
	})
	sha, err := c.CommitSHA(context.Background(), "acme", "widget", "v1")
	require.NoError(t, err)
	assert.Equal(t, paritySHA, sha)
	for _, req := range *requests {
		if req.Method == http.MethodHead {
			assert.Empty(t, req.Header.Get("Authorization"), "the probe needs no credential")
		}
	}
}

// A rate limit on the credential-free retry is an anonymous one, whatever
// token the client holds, and it keeps the IP policy behind it.
func TestPublicClient_IPAllowListRetryRateLimited(t *testing.T) {
	c, _ := publicIPAllowListClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	_, err := c.CommitSHA(context.Background(), "acme", "widget", "v1")
	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "err = %v", err)
	assert.True(t, IPAllowListBlocked(err))
	assert.False(t, RetryAt(err).IsZero(), "reset time was dropped")
	assert.Equal(t, publicAnonymous, publicAuthOf(cause(err)), "the retry used no token")
	assert.Contains(t, Brief(err), "IP allow list")
	assert.NotContains(t, Brief(err), "gh auth login", "a token can't lift an IP allow list")
}

// Which policy refused a lookup's token is evidence about that lookup. A
// concurrent lookup of the same owner that SAML refused must not change how
// the in-flight IP-policy lookup's rate limit is reported, nor the other way
// round.
func TestFallbackPolicyEvidenceIsPerLookup(t *testing.T) {
	const saml = "Resource protected by organization SAML enforcement."
	for _, tt := range []struct {
		name, slowRef, fastRef string
		slowIP                 bool
	}{
		{"IP policy in flight, SAML concurrent", "ip", "saml", true},
		{"SAML in flight, IP policy concurrent", "saml", "ip", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetAnonFallbackState()
			started, release := make(chan struct{}), make(chan struct{})
			// Not a httpmock.Registry: it serializes requests, so a request
			// held open would block the concurrent lookup.
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") != "" {
					message := ipAllowListDeniedMessage("acme")
					if req.URL.Path == "/repos/acme/widget/commits/saml" {
						message = saml
					}
					resp, err := httpmock.JSONResponse(map[string]any{"message": message})(req)
					resp.StatusCode = http.StatusForbidden
					return resp, err
				}
				switch req.URL.Path {
				case "/orgs/acme":
					return httpmock.StatusResponse(http.StatusOK)(req)
				case "/repos/acme/widget/commits/" + tt.slowRef:
					close(started)
					<-release
					return httpmock.StatusResponse(http.StatusTooManyRequests)(req)
				}
				return httpmock.JSONResponse(map[string]any{"sha": paritySHA})(req)
			})
			c, err := New("github.com", WithClientTransport(rt))
			require.NoError(t, err)
			c.anonHTTP = &http.Client{Transport: rt}

			slow := make(chan error, 1)
			go func() {
				_, err := c.CommitSHA(context.Background(), "acme", "widget", tt.slowRef)
				slow <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the in-flight lookup never reached the anonymous read")
			}
			_, err = c.CommitSHA(context.Background(), "acme", "widget", tt.fastRef)
			require.NoError(t, err)
			close(release)

			err = <-slow
			var limited *SSORateLimitedError
			require.ErrorAs(t, err, &limited, "err = %v", err)
			assert.Equal(t, tt.slowIP, IPAllowListBlocked(err), "err = %v", err)
		})
	}
}

// A rate limit that a SAML-refused metadata read hit is reported by that read
// as SAML's. It must not turn the IP-policy lookup that triggered it into an
// SSO failure: the lookup stays a retryable IP-policy rate limit.
func TestIPAllowListFallback_SAMLMetadataRateLimit(t *testing.T) {
	resetAnonFallbackState()
	reg := &httpmock.Registry{}
	reg.RegisterReusable(anyRequest, func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			message := ipAllowListDeniedMessage("acme")
			if req.URL.Path != "/graphql" {
				message = "Resource protected by organization SAML enforcement."
			}
			resp, err := httpmock.JSONResponse(map[string]any{"message": message})(req)
			resp.StatusCode = http.StatusForbidden
			return resp, err
		}
		if req.URL.Path == "/orgs/acme" {
			return httpmock.StatusResponse(http.StatusOK)(req)
		}
		return httpmock.StatusResponseWithHeaders(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"})(req)
	})
	c := newTestClient(t, reg)
	c.anonHTTP = &http.Client{Transport: reg}

	err := c.CheckPins(context.Background(), []PinCheck{{Owner: "acme", Repo: "widget", SHA: paritySHA}})[0].Err
	require.Error(t, err)
	assert.True(t, IPAllowListBlocked(err), "err = %v", err)
	assert.True(t, IsRateLimited(err), "err = %v", err)
	assert.False(t, RetryAt(err).IsZero(), "reset time was dropped")
	assert.NotContains(t, Brief(err), "SSO")
}

// After a transfer the requested owner is not the organization whose allow list
// refused the token, so IP-policy messages name no organization, while SAML's
// (which belongs to the requested owner) still does.
func TestSSORateLimitedError_NamesOwnerOnlyForSAML(t *testing.T) {
	ip := (&SSORateLimitedError{Host: "github.com", Owner: "alice", ipPolicy: true}).Error()
	assert.NotContains(t, ip, "alice")
	assert.Contains(t, ip, "IP allow list")
	assert.Contains(t, ip, "anonymous fallback for public repositories was rate-limited")

	saml := (&SSORateLimitedError{Host: "github.com", Owner: "alice"}).Error()
	assert.Contains(t, saml, "alice")
	assert.NotContains(t, saml, "IP allow list")

	c, _ := ipAllowListClient(t, "acme", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/alice" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.CommitSHA(context.Background(), "alice", "action", "v1")
	require.Error(t, err)
	assert.True(t, IPAllowListBlocked(err), "err = %v", err)
	assert.NotContains(t, Brief(err), "alice")
}
