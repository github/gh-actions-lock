package ghapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotRedirects(t *testing.T) {
	for _, tt := range []struct {
		name      string
		location  string
		wantError bool
	}{
		{name: "relative rename preserves commit", location: "/repos/acme/new/contents/ci.yml?ref=" + remoteContentsSHA},
		{name: "same origin rename preserves commit", location: "https://api.github.com/repos/acme/new/contents/ci.yml?ref=" + remoteContentsSHA},
		{name: "different host", location: "https://other.example/ci.yml?ref=" + remoteContentsSHA, wantError: true},
		{name: "subdomain", location: "https://other.api.github.com/ci.yml?ref=" + remoteContentsSHA, wantError: true},
		{name: "downgrade", location: "http://api.github.com/ci.yml?ref=" + remoteContentsSHA, wantError: true},
		{name: "credentials", location: "https://user@api.github.com/ci.yml?ref=" + remoteContentsSHA, wantError: true},
		{name: "dropped ref", location: "/repos/acme/new/contents/ci.yml", wantError: true},
		{name: "changed ref", location: "/repos/acme/new/contents/ci.yml?ref=main", wantError: true},
		{name: "malformed location", location: "https://%", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					resp, err := httpmock.StatusResponse(http.StatusMovedPermanently)(req)
					resp.Header.Set("Location", tt.location)
					return resp, err
				}
				return httpmock.JSONResponse(map[string]any{"type": "file", "encoding": "base64", "content": "Y2k="})(req)
			})
			c, err := New("github.com", WithClientTransport(rt))
			require.NoError(t, err)
			body, err := c.ReadFile(WithSnapshotRedirects(context.Background()), "octo", "old", remoteContentsSHA, "ci.yml")
			if tt.wantError {
				require.ErrorContains(t, err, "refusing API redirect")
				assert.Equal(t, 1, calls, "redirect must be refused before sending any second request")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "ci", string(body))
				assert.Equal(t, 2, calls)
			}
		})
	}
}
