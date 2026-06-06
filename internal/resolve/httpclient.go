package resolve

import (
	"context"
	"net/http"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// defaultSleep waits d but returns early when ctx is canceled. Used as the
// default sleepFn in newly constructed resolvers so the rate-limit retry
// loop honors cancellation.
func defaultSleep(ctx context.Context, d time.Duration) {
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
		client:            client,
		restClient:        restClient,
		hostname:          hostname,
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		nowFn:             time.Now,
		sleepFn:           defaultSleep,
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
