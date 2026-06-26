// Package ghapi provides a unified GitHub API client that owns both REST
// and GraphQL connections, retry transport, and profiling instrumentation.
// All GitHub API access in the CLI should go through a single Client
// instance constructed at startup.
package ghapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/syncmap"
	"golang.org/x/sync/singleflight"
)

// Client holds authenticated REST and GraphQL clients for a single
// GitHub hostname. Construct via New with ClientOption values.
type Client struct {
	graphql  *api.GraphQLClient
	rest     *api.RESTClient
	Hostname string

	// Caches for raw GitHub resources. These live here so all consumers
	// (resolver, auditor, tag lister) share a single cache per CLI run.
	compareCache         syncmap.Map[Compare, bool]
	compareSF            singleflight.Group
	repoMetaCache        syncmap.Map[Repo, repoMeta]
	repoMetaSF           singleflight.Group
	branchListCache      syncmap.Map[Repo, []BranchHead]
	branchListSF         singleflight.Group
	tagListCache         syncmap.Map[Repo, []TagEntry]
	tagListSF            singleflight.Group
	namedBranchCache     syncmap.Map[NWOName, BranchHead]
	namedBranchSF        singleflight.Group
	protectedBranchCache syncmap.Map[Repo, []BranchHead]
	protectedBranchSF    singleflight.Group
}

// ClientOption configures a Client at construction time. Pass to New.
type ClientOption func(*clientConfig)

type clientConfig struct {
	transport http.RoundTripper
	profile   *profile.Session
	authToken string // non-empty → use explicit token (tests)
	logIgnore bool   // suppress log env vars (tests)
}

// WithClientTransport overrides the HTTP transport. Use in tests with httpmock.
func WithClientTransport(t http.RoundTripper) ClientOption {
	return func(c *clientConfig) {
		c.transport = t
		c.authToken = "test-placeholder-token"
		c.logIgnore = true
	}
}

// WithClientProfile attaches profiling instrumentation to API calls.
func WithClientProfile(p *profile.Session) ClientOption {
	return func(c *clientConfig) { c.profile = p }
}

// New creates an authenticated Client for the given hostname using the
// ambient gh credential store. Use WithClientTransport for test stubs
// and WithClientProfile for profiling.
func New(hostname string, opts ...ClientOption) (*Client, error) {
	if hostname == "" {
		hostname = "github.com"
	}

	var cfg clientConfig
	for _, o := range opts {
		o(&cfg)
	}

	c := &Client{Hostname: hostname}

	apiOpts := api.ClientOptions{Host: hostname}

	switch {
	case cfg.transport != nil:
		apiOpts.Transport = cfg.transport
		apiOpts.AuthToken = cfg.authToken
		apiOpts.LogIgnoreEnv = cfg.logIgnore
	default:
		base := http.DefaultTransport
		// Integration tests: skip TLS verification when GH_ACTIONS_LOCK_INSECURE is set.
		if os.Getenv("GH_ACTIONS_LOCK_INSECURE") != "" {
			base = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // integration tests only
			}
		}
		var t http.RoundTripper = newRetryTransport(base, 3)
		if cfg.profile != nil {
			t = cfg.profile.WrapTransport(t)
		}
		apiOpts.Transport = t
	}

	gql, err := api.NewGraphQLClient(apiOpts)
	if err != nil {
		return nil, err
	}

	rest, err := api.NewRESTClient(apiOpts)
	if err != nil {
		return nil, err
	}

	c.graphql = gql
	c.rest = rest
	return c, nil
}

// retryTransport wraps an http.RoundTripper with retry logic for transient
// server errors (5xx), explicit rate limits (429), and GitHub secondary
// rate limits (403 with Retry-After or X-RateLimit-Reset headers).
type retryTransport struct {
	inner      http.RoundTripper
	maxRetries int
	sleepFn    func(context.Context, time.Duration) // for testing; defaults to DefaultSleep
}

func newRetryTransport(t http.RoundTripper, maxRetries int) http.RoundTripper {
	if t == nil {
		t = http.DefaultTransport
	}
	return &retryTransport{inner: t, maxRetries: maxRetries}
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Snapshot the body so POST requests can be replayed on retry.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body for retry: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}

	var resp *http.Response
	var err error

	for attempt := 0; attempt <= rt.maxRetries; attempt++ {
		if attempt > 0 && bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}
		resp, err = rt.inner.RoundTrip(req)
		if err != nil {
			if req.Context().Err() != nil {
				return nil, err
			}
			if attempt < rt.maxRetries {
				rt.backoff(req.Context(), attempt)
				if req.Context().Err() != nil {
					return nil, req.Context().Err()
				}
				continue
			}
			return nil, err
		}
		if rt.shouldRetry(resp) {
			if attempt < rt.maxRetries {
				wait := rt.retryWait(resp, attempt)
				resp.Body.Close()
				rt.sleep(req.Context(), wait)
				if req.Context().Err() != nil {
					return nil, req.Context().Err()
				}
				continue
			}
		}
		return resp, nil
	}
	return resp, err
}

// shouldRetry returns true for responses that warrant a retry: 5xx server
// errors, 429 explicit rate limits, and 403 secondary rate limits (indicated
// by Retry-After or X-RateLimit-Remaining: 0).
func (rt *retryTransport) shouldRetry(resp *http.Response) bool {
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode == http.StatusForbidden {
		if resp.Header.Get("Retry-After") != "" {
			return true
		}
		if resp.Header.Get("X-RateLimit-Reset") != "" && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
	}
	return false
}

// retryWait picks the sleep duration before the next attempt, honoring
// Retry-After and X-RateLimit-Reset headers when present.
func (rt *retryTransport) retryWait(resp *http.Response, attempt int) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 120 {
			return time.Duration(secs) * time.Second
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
			if d := time.Until(time.Unix(ts, 0)) + time.Second; d > 0 && d <= 120*time.Second {
				return d
			}
		}
	}
	delay := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	return delay
}

func (rt *retryTransport) sleep(ctx context.Context, d time.Duration) {
	if rt.sleepFn != nil {
		rt.sleepFn(ctx, d)
		return
	}
	DefaultSleep(ctx, d)
}

func (rt *retryTransport) backoff(ctx context.Context, attempt int) {
	delay := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	rt.sleep(ctx, delay)
}

// DefaultSleep waits d but returns early when ctx is canceled. Useful for
// rate-limit retry loops that need to honor cancellation.
func DefaultSleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
