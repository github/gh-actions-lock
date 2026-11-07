package ghapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/stretchr/testify/assert"
)

func TestBrief(t *testing.T) {
	requestURL, err := url.Parse("https://api.github.com/repos/acme/widget/commits/v1")
	assert.NoError(t, err)
	const saml = "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ordinary HTTP failure omits message and request URL",
			err:  &api.HTTPError{StatusCode: http.StatusServiceUnavailable, RequestURL: requestURL, Message: "upstream unavailable"},
			want: "GitHub returned 503 Service Unavailable",
		},
		{
			name: "ordinary forbidden failure remains concise",
			err:  &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: "Resource not accessible by integration"},
			want: "GitHub returned 403 Forbidden",
		},
		{
			name: "wrapped SAML failure retains authorization instructions",
			err:  fmt.Errorf("fetching commit: %w", &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: saml}),
			want: "GitHub returned 403 Forbidden: " + saml,
		},
		{
			name: "SAML SSO failure retains authorization instructions",
			err:  &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: "SAML SSO required. Authorize your token and retry."},
			want: "GitHub returned 403 Forbidden: SAML SSO required. Authorize your token and retry.",
		},
		{
			name: "wrapped rate-limited SSO fallback retains authorization URL",
			err: fmt.Errorf("fetching commit: %w", &SSORateLimitedError{
				Host: "github.com", Owner: "acme",
				Err: &api.HTTPError{StatusCode: http.StatusTooManyRequests, RequestURL: requestURL},
			}),
			want: ssoRequiredMessage("github.com", "acme") + " (the anonymous fallback for public repositories was rate-limited)",
		},
		{
			name: "wrapped IP allow list 403 keeps GitHub's message",
			err:  fmt.Errorf("fetching commit: %w", &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: ipAllowListDeniedMessage("acme")}),
			want: "GitHub returned 403 Forbidden: " + ipAllowListDeniedMessage("acme"),
		},
		{
			name: "GraphQL SSO guidance is unchanged",
			err:  errors.New(ssoRequiredMessage("github.com", "acme")),
			want: ssoRequiredMessage("github.com", "acme"),
		},
		{
			name: "non-HTTP error is unchanged",
			err:  errors.New("connection refused"),
			want: "connection refused",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := &BriefError{Msg: Brief(tt.err), Err: tt.err}
			assert.Equal(t, tt.want, wrapped.Error())
			assert.NotContains(t, wrapped.Error(), requestURL.String())
			assert.ErrorIs(t, wrapped, tt.err)
			assert.Equal(t, tt.err.Error(), wrapped.Verbose())
			if code, ok := StatusCode(tt.err); ok {
				got, found := StatusCode(wrapped)
				assert.True(t, found)
				assert.Equal(t, code, got)
			}
		})
	}
}

func TestOutage(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	tests := []struct {
		name        string
		err         error
		want        string
		rateLimited bool
	}{
		{"connection refused", &UnknownOutcomeError{Cause: refused}, "GitHub API unreachable (connection refused)", false},
		{"server error", &api.HTTPError{StatusCode: http.StatusBadGateway}, "GitHub API returned 502 Bad Gateway", false},
		{"rate limit", &api.HTTPError{StatusCode: http.StatusTooManyRequests}, "GitHub API rate limit exhausted", true},
		{"graphql rate limit", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "RATE_LIMITED"}}}, "GitHub API rate limit exhausted", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, limited := Outage(tt.err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.rateLimited, limited)
		})
	}
}

// TestOutage_PicksCause: in a joined error the rate limit that resets last
// decides the reason and retry time, whatever its position, and an outage
// beats a permission answer.
func TestOutage_PicksCause(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	early := time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC)
	late := early.Add(5 * time.Minute)
	limited := func(at time.Time) error {
		return &UnknownOutcomeError{Cause: &api.HTTPError{StatusCode: http.StatusTooManyRequests}, RetryAt: at}
	}
	forbidden := &api.HTTPError{StatusCode: http.StatusForbidden}
	tests := []struct {
		name        string
		err         error
		wantReason  string
		wantLimited bool
		wantAt      time.Time
	}{
		{"403 before a rate limit", errors.Join(forbidden, limited(late)), "GitHub API rate limit exhausted", true, late},
		{"rate limit before an outage, under a wrapper", fmt.Errorf("resolving: %w", errors.Join(refused, errors.Join(limited(early), limited(late)))), "GitHub API rate limit exhausted", true, late},
		{"403 before an outage", errors.Join(forbidden, refused), "GitHub API unreachable (connection refused)", false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, lim := Outage(tt.err)
			assert.Equal(t, tt.wantReason, reason)
			assert.Equal(t, tt.wantLimited, lim)
			assert.Equal(t, tt.wantAt, RetryAt(tt.err))
			assert.True(t, IsTransient(tt.err))
		})
	}
}
