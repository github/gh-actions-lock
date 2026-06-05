package resolver

import (
	"net/http"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-actions-pin/internal/cachekey"
)

// New creates a resolver using the authenticated gh context.
func New(hostname string) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{Host: hostname})
}

// NewWithOptions creates a resolver using the provided client options.
func NewWithOptions(opts api.ClientOptions) (*Resolver, error) {
	hostname := opts.Host
	if hostname == "" {
		hostname = "github.com"
	}
	opts.Host = hostname

	// Wrap the transport with retry logic for transient 5xx/429 errors.
	if opts.Transport == nil {
		opts.Transport = newRetryTransport(http.DefaultTransport, 3)
	}

	client, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}

	restClient, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}

	return &Resolver{
		client:               client,
		restClient:           restClient,
		hostname:             hostname,
		MaxRecursionDepth:    DefaultMaxRecursionDepth,
		cache:                make(map[cachekey.ActionRef]resolvedEntry),
		latestRefCache:       make(map[cachekey.Repo]string),
		reachCache:           make(map[cachekey.Reach]reachCacheEntry),
		branchListCache:      make(map[cachekey.Repo][]branchHead),
		tagListCache:         make(map[cachekey.Repo][]tagEntry),
		repoIDsCache:         make(map[cachekey.Repo][2]int64),
		defaultBranchCache:   make(map[cachekey.Repo]string),
		compareCache:         make(map[cachekey.Compare]bool),
		branchHintBySHA:      make(map[cachekey.NWOSha]string),
		namedBranchCache:     make(map[cachekey.NWOName]branchHead),
		protectedBranchCache: make(map[cachekey.Repo][]branchHead),
		releaseBranchCache:   make(map[cachekey.Repo][]branchHead),
		tagObjectCache:       make(map[cachekey.NWOSha]tagPeel),
		nowFn:                time.Now,
		sleepFn:              time.Sleep,
	}, nil
}

// NewWithTransport creates a resolver with a custom HTTP transport and a
// placeholder auth token. Intended for tests that stub HTTP responses.
func NewWithTransport(hostname string, transport http.RoundTripper) (*Resolver, error) {
	return NewWithOptions(api.ClientOptions{
		AuthToken:    "test-placeholder-token",
		Host:         hostname,
		Transport:    transport,
		LogIgnoreEnv: true,
	})
}
