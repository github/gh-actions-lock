// Package ghapi provides a unified GitHub API client that owns both REST
// and GraphQL connections, retry transport, and profiling instrumentation.
// All GitHub API access in the CLI should go through a single Client
// instance constructed at startup.
package ghapi

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-lock/internal/profile"
	"github.com/github/gh-actions-lock/internal/syncmap"
	"golang.org/x/sync/singleflight"
)

// Client holds authenticated REST and GraphQL clients for a single
// GitHub hostname. Construct via New with ClientOption values.
type Client struct {
	budget   *retryBudget
	graphql  *api.GraphQLClient
	rest     *api.RESTClient
	Hostname string
	restOnly bool
	local    *Client
	public   *Client
	routes   syncmap.Map[Repo, *Client]
	routeSF  singleflight.Group
	pinned   map[Repo]string

	// publicAuth is how the github.com public fallback client of a
	// *.ghe.com run authenticates (see publicCredential); "" otherwise.
	publicAuth string

	// anonBaseURL overrides the base URL for anonymous REST fallback calls.
	// Empty uses the default "https://api.<Hostname>". Set in tests.
	anonBaseURL string

	// anonHTTP is the HTTP client used for anonymous (unauthenticated)
	// fallback requests. Configured at construction to honor
	// GH_ACTIONS_LOCK_INSECURE (TLS skip) for integration tests.
	anonHTTP *http.Client

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

	// gitRefCache holds exact ref lookups (see gitRef), found or not, for
	// the invocation; failed lookups are not cached.
	gitRefCache syncmap.Map[NWOName, gitRefResult]
	gitRefSF    singleflight.Group
}

// ClientOption configures a Client at construction time. Pass to New.
type ClientOption func(*clientConfig)

type clientConfig struct {
	transport http.RoundTripper
	profile   *profile.Session
	authToken string // non-empty → use explicit token (tests)
	logIgnore bool   // suppress log env vars (tests)
	public    bool
	budget    *retryBudget
	// noStoredLogin skips the stored github.com login lookup (tests).
	noStoredLogin bool
}

// WithClientTransport overrides the HTTP transport. Use in tests with httpmock.
func WithClientTransport(t http.RoundTripper) ClientOption {
	return func(c *clientConfig) {
		c.transport = t
		c.authToken = "test-placeholder-token"
		c.logIgnore = true
		c.noStoredLogin = true
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
	hostname = strings.ToLower(hostname)
	if hostname == "" {
		hostname = "github.com"
	}
	// One retry budget per run: the public fallback client shares it.
	budget := newRetryBudget()
	opts = append(opts[:len(opts):len(opts)], func(c *clientConfig) { c.budget = budget })
	local, err := newClient(hostname, opts...)
	if err != nil || !IsProxima(hostname) {
		return local, err
	}
	publicOpts := append([]ClientOption(nil), opts...)
	publicOpts = append(publicOpts, func(c *clientConfig) { c.public = true })
	public, err := newClient("github.com", publicOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{Hostname: hostname, local: local, public: public, pinned: map[Repo]string{}, budget: budget}, nil
}

// DependabotProxy reports whether the CLI runs inside Dependabot, behind its
// REST-only proxy. Dependabot reads back only actions.lock and does not
// retry a failed run.
func DependabotProxy() bool {
	return os.Getenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY") == "1"
}

func newClient(hostname string, opts ...ClientOption) (*Client, error) {
	var cfg clientConfig
	for _, o := range opts {
		o(&cfg)
	}

	c := &Client{
		Hostname: hostname,
		restOnly: DependabotProxy(),
		budget:   cfg.budget,
	}

	apiOpts := api.ClientOptions{Host: hostname}

	switch {
	case cfg.transport != nil:
		apiOpts.Transport = cfg.transport
		apiOpts.AuthToken = cfg.authToken
		apiOpts.LogIgnoreEnv = cfg.logIgnore
	default:
		// Integration tests: skip TLS verification when GH_ACTIONS_LOCK_INSECURE is set.
		retry := newRetryTransport(apiTransport(os.Getenv("GH_ACTIONS_LOCK_INSECURE") != ""), cfg.budget)
		c.anonHTTP = &http.Client{Transport: retry}
		var t http.RoundTripper = retry
		if cfg.profile != nil {
			t = cfg.profile.WrapTransport(t)
		}
		apiOpts.Transport = t
	}
	if cfg.public {
		// The public fallback never inherits the tenant's credential.
		token, auth := publicCredential(cfg.noStoredLogin)
		c.publicAuth = auth
		apiOpts.AuthToken = "anonymous"
		if token != "" {
			apiOpts.AuthToken = token
		}
		if token == "" && !DependabotProxy() {
			var err error
			apiOpts.Transport, err = withPublicCache(apiOpts.Transport)
			if err != nil {
				return nil, err
			}
		}
		apiOpts.Transport = publicTransport{inner: apiOpts.Transport, token: token}
		c.restOnly = true
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

	// The anonymous SSO-fallback client shares the retry transport; test
	// transports leave it on http.DefaultClient so tests can stub it.
	if c.anonHTTP == nil {
		c.anonHTTP = http.DefaultClient
	}
	if cfg.public {
		c.anonHTTP = &http.Client{Transport: apiOpts.Transport}
	}

	return c, nil
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
